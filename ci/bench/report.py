#!/usr/bin/env python3
"""ベンチ run の状態から PR コメントの本文を作る / 結果を results.jsonl の 1 行にする。

  report.py render   # markdown を stdout へ
  report.py record   # results.jsonl に追記する JSON 1 行を stdout へ
  report.py status   # commit status の state / description を JSON で

env: STATE_DIR（meta.json, stage, job.json, integrity.txt, foreign.txt, init_time, stack, measure/summary.md）, HISTORY（results.jsonl）

ベンチの種類（meta.json / job.json の bench）:
  official       本番スタック + ポータルの公式ベンチ（main、workflow_dispatch）。job_id はポータルのジョブ番号
  isureal-bench  複製スタック + 自作ベンチ（PR の /bench）。job_id は GitHub Actions の run_id。絶対値は公式と一致しない
比較（前回比・main 比）は同じ種類の run どうしで行う。同じ commit を両方で測った組があれば「較正」（公式/自作 の比）を出し、
results.jsonl にも pair_* として残す。
"""
import json
import os
import re
import statistics
import sys
from datetime import datetime, timedelta, timezone

STATE = os.environ["STATE_DIR"]
HISTORY = os.environ.get("HISTORY", "")
JST = timezone(timedelta(hours=9))
MARK = "<!-- isucon-bench -->"
OFFICIAL = "official"
REPLICA = "isureal-bench"


def read(name, default=""):
    try:
        with open(os.path.join(STATE, name), encoding="utf-8", errors="replace") as f:
            return f.read().strip()
    except FileNotFoundError:
        return default


def history():
    if not HISTORY or not os.path.exists(HISTORY):
        return []
    with open(HISTORY) as f:
        return [json.loads(line) for line in f if line.strip()]


meta = json.loads(read("meta.json", "{}"))
job = json.loads(read("job.json", "null") or "null")
stage = read("stage", "queued")
integrity = read("integrity.txt")
foreign = read("foreign.txt")
BENCH = meta.get("bench") or (job or {}).get("bench") or OFFICIAL
STACK = read("stack") or (job or {}).get("stack") or meta.get("stack") or ""


def kind_of(h):
    return h.get("bench") or OFFICIAL


def same_kind(hist):
    return [h for h in hist if kind_of(h) == BENCH]


def short(sha):
    return (sha or "")[:7]


def sha_link(repo, sha):
    return f"[`{short(sha)}`](https://github.com/{repo}/commit/{sha})" if sha else "-"


def job_link(kind, jid, run_url=None):
    """公式はポータルのジョブ、自作ベンチは Actions の run へ。"""
    if kind == OFFICIAL:
        return f"[#{jid}](https://portal.isucon.net/contestant/benchmark_jobs/{jid})"
    return f"[run {jid}]({run_url})" if run_url else f"run {jid}"


def hist_link(h):
    return job_link(kind_of(h), h.get("job_id"), h.get("run_url"))


def bench_label(kind=None, stack=None):
    kind = kind or BENCH
    if kind == OFFICIAL:
        return "公式ベンチ（本番）"
    return "自作ベンチ（複製スタック" + (f" {stack}" if stack else "") + "）"


def region_days(stdout):
    """「asia 3 日目: 131 人」「europe 最終日（グランプリ）: 54 人」→ {region: [人数...]}"""
    out = {}
    for m in re.finditer(r'msg="(asia|europe|america) ([^":]+): (\d+) 人"', stdout):
        out.setdefault(m.group(1), []).append(m.group(3))
    return out


def baseline(hist):
    """比較対象: 同じ種類の main の直近の PASS。無ければ直近の PASS。"""
    passed = [h for h in same_kind(hist) if h.get("passed")]
    mains = [h for h in passed if h.get("label") == "main"]
    return (mains or passed or [None])[-1]


def delta(score, base):
    if not base:
        return ""
    d = score - base["score"]
    pct = f" ({d / base['score'] * 100:+.1f}%)" if base["score"] else ""
    return f"{d:+d}{pct}"


def same_pr_prev(hist):
    """PR の run なら、同じ PR・同じ種類の直前の run（FAIL も含む）。PR でない run や初回は None。"""
    if not meta.get("pr_number"):
        return None
    same = [h for h in same_kind(hist) if str(h.get("pr_number") or "") == str(meta["pr_number"])
            and h.get("pr_repo") == meta.get("pr_repo")]
    return same[-1] if same else None


def compare(score, hist):
    """見出しと status に出す比較。PR の run は同じ PR の前回と比べ、初回だけ main と比べる。"""
    prev = same_pr_prev(hist)
    if prev:
        return f"前回比 {delta(score, prev)}" if prev.get("passed") else "前回は FAIL"
    base = baseline(hist)
    if not base:
        return ""
    return f"main 比 {delta(score, base)}" + ("、この PR の初回" if meta.get("pr_number") else "")


# ---- 較正（公式と自作を同じ commit で測った組） ----

def pairs(hist):
    """app_sha ごとに、公式と自作の直近の PASS が両方あれば (sha, official, replica, ratio) を返す。新しい順。"""
    latest = {}
    for h in hist:
        if not h.get("passed") or not h.get("app_sha"):
            continue
        latest.setdefault(h["app_sha"], {})[kind_of(h)] = h
    out = []
    for sha, d in latest.items():
        o, r = d.get(OFFICIAL), d.get(REPLICA)
        if o and r and r.get("score"):
            out.append({"app_sha": sha, "official": o, "replica": r, "ratio": o["score"] / r["score"]})
    out.sort(key=lambda p: max(p["official"].get("finished_at", ""), p["replica"].get("finished_at", "")), reverse=True)
    return out


def pair_for(hist, app_sha, kind):
    """この run と同じ commit を、もう一方の種類で測った直近の PASS。"""
    other = REPLICA if kind == OFFICIAL else OFFICIAL
    found = [h for h in hist if h.get("passed") and h.get("app_sha") == app_sha and kind_of(h) == other]
    return found[-1] if found else None


def calibration_ratio(hist, n=5):
    ps = pairs(hist)[:n]
    if not ps:
        return None, 0
    return statistics.median(p["ratio"] for p in ps), len(ps)


def calibration_lines(score, hist):
    """スコアの下に出す較正の行。"""
    lines = []
    pair = pair_for(hist, meta.get("app_sha"), BENCH)
    if pair:
        if BENCH == OFFICIAL:
            ratio = score / pair["score"] if pair.get("score") else None
            other = f"同じ commit の自作ベンチ **{pair['score']}**（{hist_link(pair)}、{pair.get('stack') or '複製'}）"
        else:
            ratio = pair["score"] / score if score else None
            other = f"同じ commit の公式ベンチ **{pair['score']}**（{hist_link(pair)}）"
        lines.append(f"較正: {other}" + (f"、公式/自作 = **{ratio:.3f}**" if ratio else ""))
    ratio, n = calibration_ratio(hist)
    if BENCH == REPLICA:
        if ratio:
            lines.append(f"公式換算の目安: **{int(score * ratio):,}**（直近 {n} 組の公式/自作 の中央値 {ratio:.3f}。絶対値は本番と一致しないので差を見る）")
        elif not pair:
            lines.append("較正データなし（同じ commit を `gh workflow run bench.yml -f container_ref=<sha>` で公式でも測ると公式/自作 の比が出る）")
    return lines


def replica_detail(res):
    """自作ベンチの内訳（replica_job.py の result.detail）。"""
    d = res.get("detail") or {}
    if not d:
        return []
    lines = ["", "<details><summary>自作ベンチの内訳（配点別・リージョン別・エンドポイント別・read-after-write）</summary>", ""]
    by = d.get("byRegion") or {}
    if by:
        lines.append("リージョン別: " + "、".join(f"{r} {int(v):,}" for r, v in sorted(by.items())))
    meta_items = []
    for k, label in (("users", "利用者"), ("posts", "投稿"), ("totalRequests", "要求"), ("errorCount", "エラー")):
        if d.get(k) is not None:
            meta_items.append(f"{label} {d[k]:,}")
    if d.get("errorRate") is not None:
        meta_items.append(f"エラー率 {d['errorRate'] * 100:.2f}%")
    if d.get("loadSec") is not None:
        meta_items.append(f"負荷 {d['loadSec']:.0f} 秒")
    if d.get("initializeSec") is not None:
        meta_items.append(f"ベンチの initialize {d['initializeSec']:.1f} 秒")
    if d.get("language"):
        meta_items.append(f"language `{d['language']}`")
    if meta_items:
        lines.append("　".join(meta_items))
    raw = [("友達フィードに投稿が 1 回目で見えず再試行", d.get("lateFeedVisibility")),
           ("キャプション変更後に古い値", d.get("staleCaption")),
           ("プロフィール変更後に古い値", d.get("staleProfile"))]
    raw = [(k, v) for k, v in raw if v is not None]
    if raw:
        warn = any(v for _, v in raw)
        lines.append(("⚠️ read-after-write の疑い: " if warn else "read-after-write: ") +
                     "、".join(f"{k} {v}" for k, v in raw))
    br = d.get("breakdown") or []
    if br:
        lines += ["", "| 配点 | 件数 | 点 |", "|---|---:|---:|"]
        lines += [f"| {b.get('kind')} | {b.get('count', 0):,} | {b.get('points', 0):,.0f} |" for b in br if b.get("count")]
    days = d.get("daysUsers") or {}
    if days:
        lines += ["", "| region | 1 日目→最終日の人数 | 検証通過 |", "|---|---|---|"]
        passed = d.get("daysPassed") or {}
        gp = d.get("grandPrixPassed") or {}
        for r in sorted(days):
            lines.append(f"| {r} | {' → '.join(str(x) for x in days[r])} | "
                         f"{' → '.join(str(x) for x in passed.get(r, []))}（最終日 {gp.get(r, 0)}） |")
    reqs = d.get("requests") or []
    if reqs:
        lines += ["", "| endpoint | count | errors | p50 ms | p95 ms | max ms |", "|---|---:|---:|---:|---:|---:|"]
        lines += [f"| {r.get('label')} | {r.get('count', 0):,} | {r.get('errors', 0):,} | {r.get('p50_ms', 0):.0f} | "
                  f"{r.get('p95_ms', 0):.0f} | {r.get('max_ms', 0):.0f} |" for r in reqs[:20]]
    lines += ["", "</details>"]
    return lines


def render():
    lines = [MARK, f"### ISUCON bench: {meta.get('label', '?')} · {bench_label(stack=STACK)}"]
    lines.append(
        f"app {sha_link(meta.get('app_repo'), meta.get('app_sha'))} / "
        f"container {sha_link(meta.get('container_repo'), meta.get('container_sha'))} / "
        f"[run]({meta.get('run_url')})"
        + (f" / note: {meta['note']}" if meta.get("note") else "")
    )
    if meta.get("fallback"):
        lines.append(f"\n> [!NOTE]\n> {meta['fallback']}")
    lines.append("")

    kind, _, arg = stage.partition(":")
    if kind == "queued":
        if BENCH == OFFICIAL:
            lines.append(f"⏳ キュー待ち（前に {arg or '?'} 件）")
        else:
            lines.append(f"⏳ 複製スタックの空き待ち（{arg or '?'} 台とも使用中）" if arg else "⏳ 複製スタックを探しています")
    elif kind == "deploying":
        lines.append(f"🚚 デプロイ中（{STACK or 'isucon-prod-2'}）")
    elif kind == "benchmarking":
        if BENCH == OFFICIAL:
            lines.append(f"🏃 ベンチ実行中 [#{arg}](https://portal.isucon.net/contestant/benchmark_jobs/{arg})")
        else:
            lines.append(f"🏃 自作ベンチ実行中（複製スタック {arg}）")
    elif kind == "failed":
        lines.append(f"💥 `{arg}` で失敗しました。[run のログ]({meta.get('run_url')})を見てください。")
        if not job:
            lines.append("ベンチは投げていません（スコア記録なし）。")
    elif kind == "rejected":
        lines.append(f"🚫 {arg}")

    if job:
        res = job.get("result") or {}
        ok = res.get("passed")
        score = job.get("score", 0)
        all_hist = [h for h in history() if not (h.get("job_id") == job["id"] and kind_of(h) == BENCH)]
        hist = same_kind(all_hist)
        base = baseline(hist)
        if meta.get("pr_number"):
            rows = (("この PR の前回", same_pr_prev(hist)), ("baseline (main)", base))
        else:
            rows = (("baseline (main)", base), ("直前の CI ベンチ", hist[-1] if hist else None))
        cmp = compare(score, hist) if ok else ""
        jid = job["id"]
        lines += [
            "",
            f"## {'✅ PASS' if ok else '❌ FAIL'} **{score}**" + (f"　{cmp}" if cmp else ""),
            "",
            "| | job | score | app | container |",
            "|---|---|---:|---|---|",
            f"| **this** | {job_link(BENCH, jid, meta.get('run_url'))} | {score} | "
            f"{sha_link(meta.get('app_repo'), meta.get('app_sha'))} | "
            f"{sha_link(meta.get('container_repo'), meta.get('container_sha'))} |",
        ]
        for name, h in rows:
            if h:
                lines.append(
                    f"| {name}: {h.get('label')} | {hist_link(h)} | "
                    f"{h['score']}{'' if h.get('passed') else ' (FAIL)'} | "
                    f"{sha_link(h.get('app_repo'), h.get('app_sha'))} | "
                    f"{sha_link(h.get('container_repo'), h.get('container_sha'))} |"
                )
        if ok:
            cal = calibration_lines(score, all_hist)
            if cal:
                lines += [""] + [f"<sub>{l}</sub>" for l in cal]
        if job["status"] != "FINISHED":
            lines.append(f"\nジョブの状態: **{job['status']}**")
        if res.get("reason") and not ok:
            lines.append(f"\n**{res['reason']}**")
        msgs = res.get("messages") or []
        if msgs:
            lines.append("")
            lines += [f"- {m}" for m in msgs[:15]]
            if len(msgs) > 15:
                lines.append(f"- …ほか {len(msgs) - 15} 件")
        days = region_days(res.get("stdout", ""))
        if days and BENCH == OFFICIAL:
            lines += ["", "| region | 1 日目→最終日の人数 |", "|---|---|"]
            lines += [f"| {r} | {' → '.join(v)} |" for r, v in days.items()]
        gp = re.search(r'msg="(グランプリ: [^"]+)"', res.get("stdout", ""))
        if gp:
            lines.append(f"\n{gp.group(1)}")
        if BENCH == REPLICA:
            lines += replica_detail(res)

    if integrity:
        lines += ["", "> [!WARNING]", "> ベンチ中にサーバの状態が変わりました。この結果は信用できません。"]
        lines += [f"> `{l}`" for l in integrity.splitlines()]
    if foreign:
        lines += ["", "> [!WARNING]",
                  f"> CI 以外から投げられたベンチがあります: {foreign}。サーバの状態が CI の想定と違う可能性があります。"]

    init_time = read("init_time")
    if init_time:
        lines.append(f"\n<sub>CI の initialize（デプロイ直後の 1 回目）: {init_time} 秒</sub>")
    measure = read("measure/summary.md")
    if measure:
        if len(measure) > 20000:  # コメントは 65536 字まで。切ったらコードブロックを閉じる
            measure = measure[:20000] + "\n```\n（長いので省略。全体は run のログ）"
        lines += ["", "<details><summary>計測（vmstat / DB wait / pg_stat_statements / alp）</summary>", "",
                  measure, "", "</details>"]

    if job and (job.get("result") or {}).get("stdout"):
        lines += ["", f"<details><summary>{'benchmarker' if BENCH == OFFICIAL else 'isureal-bench'} stdout</summary>", "", "```",
                  job["result"]["stdout"][-6000:], "```", "</details>"]
    lines.append(f"\n<sub>{datetime.now(JST):%H:%M:%S} JST</sub>")
    return "\n".join(lines) + "\n"


def record():
    res = job.get("result") or {}
    detail = res.get("detail") or {}
    hist = [h for h in history() if not (h.get("job_id") == job["id"] and kind_of(h) == BENCH)]
    pair = pair_for(hist, meta.get("app_sha"), BENCH) if res.get("passed") else None
    score = job.get("score", 0)
    if pair and pair.get("score") and score:
        pair_ratio = (score / pair["score"]) if BENCH == OFFICIAL else (pair["score"] / score)
    else:
        pair_ratio = None
    return json.dumps({
        "job_id": job["id"],
        "bench": BENCH,
        "stack": STACK or ("isucon-prod-2" if BENCH == OFFICIAL else ""),
        "score": score,
        "passed": bool(res.get("passed")),
        "status": job["status"],
        "reason": res.get("reason", ""),
        "label": meta.get("label"),
        "pr_repo": meta.get("pr_repo"),
        "pr_number": meta.get("pr_number"),
        "app_repo": meta.get("app_repo"),
        "app_sha": meta.get("app_sha"),
        "container_repo": meta.get("container_repo"),
        "container_sha": meta.get("container_sha"),
        "run_url": meta.get("run_url"),
        "finished_at": datetime.fromtimestamp(job.get("finished_at") or 0, JST).isoformat(),
        "integrity_ok": not integrity,
        "foreign_jobs": foreign,
        "init_sec": init_sec(),
        # 較正: 同じ commit をもう一方の種類で測った直近の PASS と、公式/自作 の比
        "pair_bench": kind_of(pair) if pair else None,
        "pair_job_id": pair.get("job_id") if pair else None,
        "pair_score": pair.get("score") if pair else None,
        "pair_ratio": round(pair_ratio, 4) if pair_ratio else None,
        # 自作ベンチだけ: read-after-write の遅れの回数とエラー率
        "late_feed": detail.get("lateFeedVisibility"),
        "stale_caption": detail.get("staleCaption"),
        "stale_profile": detail.get("staleProfile"),
        "error_rate": detail.get("errorRate"),
        "load_sec": detail.get("loadSec"),
    }, ensure_ascii=False)


def init_sec():
    try:
        return float(read("init_time"))
    except ValueError:
        return None


def status():
    """commit status（context: Benchmark）の state と description。description は 140 字まで。"""
    kind, _, arg = stage.partition(":")
    if kind == "queued":
        state, desc = "pending", f"キュー待ち（前に {arg or '?'} 件）" if BENCH == OFFICIAL else "複製スタックの空き待ち"
    elif kind == "deploying":
        state, desc = "pending", "デプロイ中"
    elif kind == "benchmarking":
        state, desc = "pending", f"ベンチ実行中 #{arg}" if BENCH == OFFICIAL else f"自作ベンチ実行中（{arg}）"
    elif kind == "rejected":
        state, desc = "failure", arg
    elif kind == "done" and job:
        res = job.get("result") or {}
        score = job.get("score", 0)
        if res.get("passed"):
            hist = same_kind([h for h in history() if not (h.get("job_id") == job["id"] and kind_of(h) == BENCH)])
            cmp = compare(score, hist)
            state, desc = "success", f"PASS {score}" + (f"（{cmp}）" if cmp else "")
        else:
            state, desc = "failure", f"FAIL: {res.get('reason') or job['status']}"
        if integrity:
            state, desc = "error", "ベンチ中にサーバの状態が変化（結果は信用できない）: " + desc
    else:
        state = "error"
        desc = f"{arg or kind} で失敗" + ("" if job else "（ベンチなし）")
    return json.dumps({"state": state, "description": desc[:140]}, ensure_ascii=False)


if __name__ == "__main__":
    {"render": lambda: sys.stdout.write(render()),
     "record": lambda: print(record()),
     "status": lambda: print(status())}[sys.argv[1]]()
