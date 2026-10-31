#!/usr/bin/env python3
"""ベンチの前後で計測して比較する道具。ローカルでも GitHub Actions でも同じものを使う。

サブコマンド:
  deploy    app/webapp を 3 台に配ってビルド・再起動する（直前の状態をバックアップ）
  rollback  deploy 前の状態に戻す
  snapshot  nginx ログの位置と pg_stat_statements を記録する
  wait      ベンチの開始（POST /api/initialize）と終了を待つ
  collect   snapshot 以降のログと統計を集計して結果 JSON を作る
  compare   2 つの結果 JSON を比べて Markdown を出す
  run       snapshot → wait → collect → compare をまとめて行う（ローカル用）

ログの truncate や pg_stat_statements_reset はしない（他の人の計測を壊さないため）。
"""
from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import tarfile
import io
import time
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

REGIONS = {
    "asia": os.environ.get("BENCH_HOST_ASIA", "isucon26-asia"),
    "europe": os.environ.get("BENCH_HOST_EUROPE", "isucon26-europe"),
    "america": os.environ.get("BENCH_HOST_AMERICA", "isucon26-america"),
}
COUNTRY = {"asia": "JP", "europe": "GB", "america": "US"}
MULTIPLIER = {"asia": 1.0, "europe": 1.5, "america": 1.5}
ROUTER = os.environ.get("BENCH_ROUTER", "http://35.73.68.236:8080")
ACCESS_LOG = "/var/log/nginx/access.log"
MARKER = "~/.bench/deployed.md5"
# サーバ上のソースのハッシュ（ビルド成果物・テストは除く）
SRC_HASH_CMD = (
    "cd ~/webapp && find go sql -type f ! -name isureal ! -name isureal.new "
    "! -path 'sql/tests/*' | LC_ALL=C sort | xargs cat | md5sum | cut -d' ' -f1"
)


def log(msg: str) -> None:
    print(f"[bench {datetime.now().strftime('%H:%M:%S')}] {msg}", file=sys.stderr, flush=True)


def ssh(host: str, cmd: str, *, input_bytes: bytes | None = None, check: bool = True) -> str:
    res = subprocess.run(
        ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=15", host, cmd],
        input=input_bytes,
        capture_output=True,
    )
    if check and res.returncode != 0:
        raise SystemExit(f"ssh {host} failed ({res.returncode}): {cmd}\n{res.stderr.decode(errors='replace')}")
    return res.stdout.decode(errors="replace")


def log_size(host: str) -> int:
    return int(ssh(host, f"sudo -n stat -c %s {ACCESS_LOG}").strip() or 0)


# ---------------------------------------------------------------- deploy

def bench_running() -> bool:
    """asia1 の access.log が 5 秒で大きく伸びていればベンチ中とみなす。"""
    host = REGIONS["asia"]
    a = log_size(host)
    time.sleep(5)
    b = log_size(host)
    return b - a > 20_000


def pack_webapp(webapp: Path) -> bytes:
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tar:
        for sub in ("go", "sql"):
            root = webapp / sub
            for p in sorted(root.rglob("*")):
                rel = p.relative_to(webapp).as_posix()
                if p.is_dir() or rel in ("go/isureal", "go/isureal.new") or p.name.startswith("._"):
                    continue
                data = p.read_bytes()
                if b"\0" not in data:  # Windows で CRLF になったテキストを LF に戻す（.sh が壊れないように）
                    data = data.replace(b"\r\n", b"\n")
                info = tarfile.TarInfo(rel)
                info.size = len(data)
                info.mode = 0o755 if rel.endswith(".sh") else 0o644
                info.mtime = int(p.stat().st_mtime)
                tar.addfile(info, io.BytesIO(data))
    return buf.getvalue()


def cmd_deploy(args: argparse.Namespace) -> None:
    webapp = Path(args.webapp)
    if not (webapp / "go" / "go.mod").exists():
        raise SystemExit(f"{webapp}/go/go.mod がありません")
    if not args.force and bench_running():
        raise SystemExit("ベンチが走っているようなのでデプロイを中止しました（--force で無視）")
    for region, host in REGIONS.items():
        current = ssh(host, SRC_HASH_CMD).strip()
        marker = ssh(host, f"cat {MARKER} 2>/dev/null || true").strip()
        if marker != current and not args.force:
            raise SystemExit(
                f"{region}: サーバのソースが bench でデプロイしたものと違います（手で入れた未コミットのコードの可能性。#13）。"
                " 上書きすると消えるので中止しました。コミット済みを確認してから --force で実行してください。"
            )
    payload = pack_webapp(webapp)
    build = (
        "set -e; mkdir -p ~/.bench; rm -rf ~/.bench/backup; mkdir -p ~/.bench/backup; "
        "cp -a ~/webapp/go ~/webapp/sql ~/.bench/backup/; "
        "cp ~/.bench/deployed.md5 ~/.bench/backup/deployed.md5 2>/dev/null || true; "
        "rm -rf ~/.bench/incoming && mkdir -p ~/.bench/incoming && tar xzf - -C ~/.bench/incoming; "
        # 古いファイルが残らないように置き換え（ビルド成果物はバックアップから戻せる）
        "rm -rf ~/webapp/go ~/webapp/sql && mv ~/.bench/incoming/go ~/.bench/incoming/sql ~/webapp/; "
        "chmod +x ~/webapp/sql/*.sh 2>/dev/null || true; "
        "cd ~/webapp/go && PATH=$HOME/local/go/bin:$PATH GOEXPERIMENT=simd go build -o isureal cmd/isureal/main.go; "
        "sudo -n systemctl restart isureal-go; "
        f"{SRC_HASH_CMD} > {MARKER}; echo \"$(hostname) deployed\""
    )
    procs = {}
    for region, host in REGIONS.items():
        procs[region] = subprocess.Popen(
            ["ssh", "-o", "BatchMode=yes", host, build],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        )
        procs[region].stdin.write(payload)
        procs[region].stdin.close()
    failed = []
    for region, p in procs.items():
        out, err = p.stdout.read().decode(), p.stderr.read().decode()
        if p.wait() != 0:
            failed.append(region)
            log(f"{region}: deploy failed\n{err}")
        else:
            log(f"{region}: {out.strip()}")
    if failed:
        raise SystemExit(f"deploy failed on {failed}. `bench.py rollback` で戻せます")
    health_check()


def health_check() -> None:
    for region, cc in COUNTRY.items():
        ok = False
        for _ in range(20):
            try:
                req = urllib.request.Request(f"{ROUTER}/", headers={"X-Country-Code": cc})
                with urllib.request.urlopen(req, timeout=5) as r:
                    ok = r.status == 200
            except Exception:
                ok = False
            if ok:
                break
            time.sleep(1)
        if not ok:
            raise SystemExit(f"{region}: ルーター経由の疎通確認に失敗しました")
        log(f"{region}: healthy via router")


def cmd_rollback(args: argparse.Namespace) -> None:
    restore = (
        "set -e; test -d ~/.bench/backup/go; "
        "rm -rf ~/webapp/go ~/webapp/sql && cp -a ~/.bench/backup/go ~/.bench/backup/sql ~/webapp/; "
        f"if [ -f ~/.bench/backup/deployed.md5 ]; then cp ~/.bench/backup/deployed.md5 {MARKER}; else rm -f {MARKER}; fi; "
        "sudo -n systemctl restart isureal-go; echo \"$(hostname) restored\""
    )
    for region, host in REGIONS.items():
        log(f"{region}: {ssh(host, restore).strip()}")
    health_check()


# ---------------------------------------------------------------- measure

PG_SQL = (
    "SELECT queryid, calls, round(total_exec_time::numeric,3), "
    "left(regexp_replace(query, '\\s+', ' ', 'g'), 160) FROM pg_stat_statements"
)


def pg_rows() -> dict[str, dict]:
    cmd = (
        '. ~/env.sh; PGPASSWORD="$ISUREAL_DB_PASSWORD" psql -h "$ISUREAL_DB_HOST" -p "$ISUREAL_DB_PORT" '
        '-U "$ISUREAL_DB_USER" -d "$ISUREAL_DB_NAME" -X -A -t -F "$(printf \'\\t\')" -c "' + PG_SQL + '"'
    )
    rows = {}
    for line in ssh(REGIONS["asia"], cmd).splitlines():
        parts = line.split("\t", 3)
        if len(parts) == 4 and parts[0]:
            rows[parts[0]] = {"calls": int(parts[1]), "total_ms": float(parts[2]), "query": parts[3]}
    return rows


def cmd_snapshot(args: argparse.Namespace) -> None:
    snap = {
        "taken_at": datetime.now(timezone.utc).isoformat(),
        "offsets": {r: log_size(h) for r, h in REGIONS.items()},
        "pg": pg_rows(),
    }
    Path(args.out).write_text(json.dumps(snap), encoding="utf-8")
    log(f"snapshot saved: {args.out} offsets={snap['offsets']}")


LINE_CMD = (
    "sudo -n tail -c +{start} " + ACCESS_LOG + " | awk -F'\\t' '"
    "{{for(i=1;i<=NF;i++){{k=$i; sub(/:.*/,\"\",k); v=substr($i,length(k)+2); f[k]=v}} "
    "print f[\"time\"]\"\\t\"f[\"method\"]\"\\t\"f[\"uri\"]\"\\t\"f[\"status\"]\"\\t\"f[\"reqtime\"]}}'"
)


def read_lines(host: str, offset: int) -> list[tuple[str, str, str, int, float]]:
    size = log_size(host)
    start = offset + 1 if size >= offset else 1  # ログが truncate されていたら先頭から
    out = []
    for line in ssh(host, LINE_CMD.format(start=start)).splitlines():
        p = line.split("\t")
        if len(p) != 5:
            continue
        try:
            out.append((p[0], p[1], p[2], int(p[3]), float(p[4] or 0)))
        except ValueError:
            continue
    return out


def find_start(snap: dict) -> str | None:
    for region, host in REGIONS.items():
        for t, method, uri, _, _ in read_lines(host, snap["offsets"][region]):
            if method == "POST" and uri.startswith("/api/initialize"):
                return t
    return None


def cmd_wait(args: argparse.Namespace) -> None:
    snap = json.loads(Path(args.snap).read_text(encoding="utf-8"))
    log(f"waiting up to {args.start_timeout}s for the benchmark to start (press Run in the portal)")
    deadline = time.time() + args.start_timeout
    start = None
    while time.time() < deadline:
        start = find_start(snap)
        if start:
            break
        time.sleep(5)
    if not start:
        raise SystemExit("ベンチの開始（POST /api/initialize）が見つかりませんでした")
    log(f"benchmark started at {start}")
    t0 = time.time()
    last_total, quiet_since = -1, time.time()
    while time.time() - t0 < args.run_timeout:
        total = sum(log_size(h) for h in REGIONS.values())
        if total != last_total:
            last_total, quiet_since = total, time.time()
        elif time.time() - quiet_since >= args.quiet and time.time() - t0 >= args.min_duration:
            log("benchmark finished (logs are quiet)")
            return
        time.sleep(3)
    log("run timeout reached; collecting anyway")


def normalize(uri: str) -> str:
    uri = uri.split("?", 1)[0]
    uri = re.sub(r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}", ":id", uri)
    uri = re.sub(r"\d{4}-\d{2}-\d{2}", ":date", uri)
    return "/".join(":id" if re.search(r"\d", s) and len(s) >= 6 else s for s in uri.split("/"))


def pct(values: list[float], q: float) -> float:
    if not values:
        return 0.0
    v = sorted(values)
    return v[min(len(v) - 1, int(q * len(v)))]


def cmd_collect(args: argparse.Namespace) -> None:
    snap = json.loads(Path(args.snap).read_text(encoding="utf-8"))
    start = find_start(snap)
    regions = {}
    for region, host in REGIONS.items():
        lines = [l for l in read_lines(host, snap["offsets"][region]) if not start or l[0] >= start]
        eps: dict[str, list] = {}
        statuses: dict[str, int] = {}
        for _, method, uri, status, rt in lines:
            key = f"{method} {normalize(uri)}"
            e = eps.setdefault(key, [[], 0])
            if key != "GET /api/notifications":  # SSE は長時間接続なので時間は集計しない
                e[0].append(rt)
            if 200 <= status < 300:
                e[1] += 1
            statuses[str(status)] = statuses.get(str(status), 0) + 1
        endpoints = {
            k: {
                "count": len(v[0]) if k != "GET /api/notifications" else v[1],
                "ok": v[1],
                "total_s": round(sum(v[0]), 3),
                "avg": round(sum(v[0]) / len(v[0]), 4) if v[0] else 0,
                "p50": pct(v[0], 0.5),
                "p99": pct(v[0], 0.99),
            }
            for k, v in eps.items()
        }
        timed = [rt for (_, m, u, _, rt) in lines if f"{m} {normalize(u)}" != "GET /api/notifications"]
        regions[region] = {
            "requests": len(lines),
            "ok": sum(c for s, c in statuses.items() if s.startswith("2")),
            "errors_5xx": sum(c for s, c in statuses.items() if s.startswith("5")),
            "avg": round(sum(timed) / len(timed), 4) if timed else 0,
            "p99": pct(timed, 0.99),
            "statuses": statuses,
            "endpoints": endpoints,
        }
    before, after = snap["pg"], pg_rows()
    pg = []
    for qid, a in after.items():
        b = before.get(qid, {"calls": 0, "total_ms": 0.0})
        if a["calls"] < b["calls"]:  # 誰かが reset した
            b = {"calls": 0, "total_ms": 0.0}
        calls = a["calls"] - b["calls"]
        if calls <= 0:
            continue
        total = a["total_ms"] - b["total_ms"]
        pg.append({"query": a["query"], "calls": calls, "total_ms": round(total, 1), "mean_ms": round(total / calls, 2)})
    pg.sort(key=lambda r: -r["total_ms"])
    result = {
        "label": args.label,
        "collected_at": datetime.now(timezone.utc).isoformat(),
        "bench_start": start,
        "score": args.score,
        "regions": regions,
        "pg": pg[:40],
    }
    Path(args.out).write_text(json.dumps(result, ensure_ascii=False, indent=1), encoding="utf-8")
    log(f"result saved: {args.out}" + ("" if start else " (WARNING: no /api/initialize found)"))


# ---------------------------------------------------------------- compare

def delta(b: float, n: float, lower_is_better: bool = False) -> str:
    if not b:
        return "new" if n else "—"
    d = (n - b) / b * 100
    good = d < 0 if lower_is_better else d > 0
    mark = "🟢" if (abs(d) >= 5 and good) else ("🔴" if abs(d) >= 5 else "⚪")
    return f"{mark} {d:+.0f}%"


def weighted(r: dict) -> float:
    return sum(r["regions"][g]["ok"] * MULTIPLIER[g] for g in r["regions"])


def cmd_compare(args: argparse.Namespace) -> None:
    new = json.loads(Path(args.new).read_text(encoding="utf-8"))
    base = json.loads(Path(args.base).read_text(encoding="utf-8")) if args.base and Path(args.base).exists() else None
    out = []
    title = f"## ベンチ計測: `{new['label']}`" + (f" vs `{base['label']}`" if base else "")
    out.append(title)
    if not new.get("bench_start"):
        out.append("> ⚠️ `POST /api/initialize` が見つからなかったので、snapshot 以降の全リクエストを集計しています。")
    if new.get("score") or (base and base.get("score")):
        bs = base.get("score") if base else None
        out.append(f"\n**スコア**: {bs or '?'} → **{new.get('score') or '?'}** {delta(bs or 0, new.get('score') or 0) if bs and new.get('score') else ''}")
    out.append("\n### リージョン別（2xx 件数 × 倍率は得点の目安）")
    out.append("| region | 2xx 件数 | 平均 応答 | p99 | 5xx |")
    out.append("|---|---:|---:|---:|---:|")
    for g in REGIONS:
        n = new["regions"].get(g, {})
        b = base["regions"].get(g, {}) if base else {}
        if base:
            out.append(
                f"| {g} ×{MULTIPLIER[g]} | {b.get('ok',0)} → **{n.get('ok',0)}** {delta(b.get('ok',0), n.get('ok',0))} "
                f"| {b.get('avg',0):.3f}s → **{n.get('avg',0):.3f}s** {delta(b.get('avg',0), n.get('avg',0), True)} "
                f"| {b.get('p99',0):.2f}s → {n.get('p99',0):.2f}s | {b.get('errors_5xx',0)} → {n.get('errors_5xx',0)} |"
            )
        else:
            out.append(f"| {g} ×{MULTIPLIER[g]} | {n.get('ok',0)} | {n.get('avg',0):.3f}s | {n.get('p99',0):.2f}s | {n.get('errors_5xx',0)} |")
    wn = weighted(new)
    if base:
        wb = weighted(base)
        out.append(f"\n2xx × 倍率の合計: {wb:.0f} → **{wn:.0f}** {delta(wb, wn)}")
        out.append("\n```mermaid\nxychart-beta\n    title \"2xx 件数（棒 = 今回、線 = 比較元）\"")
        out.append(f"    x-axis [{', '.join(REGIONS)}]")
        out.append(f"    bar [{', '.join(str(new['regions'][g]['ok']) for g in REGIONS)}]")
        out.append(f"    line [{', '.join(str(base['regions'].get(g, {}).get('ok', 0)) for g in REGIONS)}]\n```")

    out.append("\n### エンドポイント別（今回の合計時間の上位）")
    merged: dict[str, dict] = {}
    for g in REGIONS:
        for k, e in new["regions"][g]["endpoints"].items():
            m = merged.setdefault(k, {"total": 0.0})
            m["total"] += e["total_s"]
            m[g] = e
    hdr = "| endpoint | " + " | ".join(f"{g} 件数 / 平均" for g in REGIONS) + " |"
    out.append(hdr)
    out.append("|---|" + "---:|" * len(REGIONS))
    for k, m in sorted(merged.items(), key=lambda kv: -kv[1]["total"])[: args.top]:
        cells = []
        for g in REGIONS:
            e = m.get(g)
            if not e:
                cells.append("—")
                continue
            be = base["regions"].get(g, {}).get("endpoints", {}).get(k) if base else None
            cell = f"{e['count']} / {e['avg']:.3f}s"
            if be:
                cell += f" {delta(be['avg'], e['avg'], True)}"
            cells.append(cell)
        out.append(f"| `{k}` | " + " | ".join(cells) + " |")

    out.append("\n### DB（pg_stat_statements、今回の合計時間の上位）")
    out.append("| 合計 | calls | 平均 | 比較元の平均 | query |")
    out.append("|---:|---:|---:|---:|---|")
    base_pg = {r["query"]: r for r in (base or {}).get("pg", [])}
    for r in new["pg"][: args.top]:
        b = base_pg.get(r["query"])
        bm = f"{b['mean_ms']:.1f}ms {delta(b['mean_ms'], r['mean_ms'], True)}" if b else "—"
        q = r["query"].replace("|", "\\|")[:110]
        out.append(f"| {r['total_ms']/1000:.1f}s | {r['calls']} | {r['mean_ms']:.1f}ms | {bm} | `{q}` |")
    out.append("\n<sub>🟢/🔴 = 5% 以上の改善/悪化。応答時間は nginx の request_time（SSE は除外）。スコアはポータルの値を手で渡したときだけ表示。</sub>")
    text = "\n".join(out) + "\n"
    if args.out:
        Path(args.out).write_text(text, encoding="utf-8")
    sys.stdout.write(text)


# ---------------------------------------------------------------- run (local)

def cmd_run(args: argparse.Namespace) -> None:
    rdir = Path(args.results)
    rdir.mkdir(parents=True, exist_ok=True)
    stamp = datetime.now().strftime("%Y%m%d-%H%M%S")
    snap = rdir / f".snap-{stamp}.json"
    if args.deploy:
        cmd_deploy(argparse.Namespace(webapp=args.webapp, force=args.force))
    cmd_snapshot(argparse.Namespace(out=str(snap)))
    print("\n>>> ポータルでベンチを実行してください: https://portal.isucon.net/contestant/benchmark_jobs\n", file=sys.stderr)
    try:
        cmd_wait(argparse.Namespace(snap=str(snap), start_timeout=args.start_timeout, run_timeout=600, quiet=20, min_duration=90))
        result = rdir / f"{stamp}-{args.label}.json"
        cmd_collect(argparse.Namespace(snap=str(snap), out=str(result), label=args.label, score=args.score))
        cmd_compare(argparse.Namespace(new=str(result), base=args.base, out=str(result.with_suffix(".md")), top=args.top))
    finally:
        snap.unlink(missing_ok=True)
        if args.deploy and args.rollback:
            cmd_rollback(argparse.Namespace())


def git_label() -> str:
    try:
        return subprocess.run(["git", "-C", "app", "rev-parse", "--short", "HEAD"], capture_output=True, text=True).stdout.strip() or "local"
    except Exception:
        return "local"


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sp = ap.add_subparsers(dest="cmd", required=True)

    p = sp.add_parser("deploy"); p.add_argument("--webapp", default="app/webapp"); p.add_argument("--force", action="store_true")
    p.set_defaults(fn=cmd_deploy)
    p = sp.add_parser("rollback"); p.set_defaults(fn=cmd_rollback)
    p = sp.add_parser("snapshot"); p.add_argument("-o", "--out", required=True); p.set_defaults(fn=cmd_snapshot)
    p = sp.add_parser("wait"); p.add_argument("--snap", required=True)
    p.add_argument("--start-timeout", type=int, default=900); p.add_argument("--run-timeout", type=int, default=600)
    p.add_argument("--quiet", type=int, default=20); p.add_argument("--min-duration", type=int, default=90)
    p.set_defaults(fn=cmd_wait)
    p = sp.add_parser("collect"); p.add_argument("--snap", required=True); p.add_argument("-o", "--out", required=True)
    p.add_argument("--label", default=git_label()); p.add_argument("--score", type=int)
    p.set_defaults(fn=cmd_collect)
    p = sp.add_parser("compare"); p.add_argument("new"); p.add_argument("--base"); p.add_argument("-o", "--out"); p.add_argument("--top", type=int, default=15)
    p.set_defaults(fn=cmd_compare)
    p = sp.add_parser("run"); p.add_argument("--deploy", action="store_true"); p.add_argument("--rollback", action="store_true")
    p.add_argument("--force", action="store_true"); p.add_argument("--webapp", default="app/webapp")
    p.add_argument("--base", default="bench/results/baseline.json"); p.add_argument("--results", default="bench/results")
    p.add_argument("--label", default=git_label()); p.add_argument("--score", type=int)
    p.add_argument("--start-timeout", type=int, default=900); p.add_argument("--top", type=int, default=15)
    p.set_defaults(fn=cmd_run)

    for stream in (sys.stdout, sys.stderr):
        stream.reconfigure(encoding="utf-8")
    args = ap.parse_args()
    args.fn(args)


if __name__ == "__main__":
    main()
