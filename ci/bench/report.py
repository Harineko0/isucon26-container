#!/usr/bin/env python3
"""ベンチ run の状態から PR コメントの本文を作る / 結果を results.jsonl の 1 行にする。

  report.py render   # markdown を stdout へ
  report.py record   # results.jsonl に追記する JSON 1 行を stdout へ

env: STATE_DIR（meta.json, stage, job.json, integrity.txt, foreign.txt）, HISTORY（results.jsonl）
"""
import json
import os
import re
import sys
from datetime import datetime, timedelta, timezone

STATE = os.environ["STATE_DIR"]
HISTORY = os.environ.get("HISTORY", "")
JST = timezone(timedelta(hours=9))
MARK = "<!-- isucon-bench -->"


def read(name, default=""):
    try:
        with open(os.path.join(STATE, name)) as f:
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


def short(sha):
    return (sha or "")[:7]


def sha_link(repo, sha):
    return f"[`{short(sha)}`](https://github.com/{repo}/commit/{sha})" if sha else "-"


def region_days(stdout):
    """「asia 3 日目: 131 人」「europe 最終日（グランプリ）: 54 人」→ {region: [人数...]}"""
    out = {}
    for m in re.finditer(r'msg="(asia|europe|america) ([^":]+): (\d+) 人"', stdout):
        out.setdefault(m.group(1), []).append(m.group(3))
    return out


def baseline(hist):
    """比較対象: main の直近の PASS。無ければ直近の PASS。"""
    passed = [h for h in hist if h.get("passed")]
    mains = [h for h in passed if h.get("label") == "main"]
    return (mains or passed or [None])[-1]


def delta(score, base):
    if not base:
        return ""
    d = score - base["score"]
    pct = f" ({d / base['score'] * 100:+.1f}%)" if base["score"] else ""
    return f"{d:+d}{pct}"


def render():
    lines = [MARK, f"### ISUCON bench: {meta.get('label', '?')}"]
    lines.append(
        f"app {sha_link(meta.get('app_repo'), meta.get('app_sha'))} / "
        f"container {sha_link(meta.get('container_repo'), meta.get('container_sha'))} / "
        f"[run]({meta.get('run_url')})"
        + (f" / note: {meta['note']}" if meta.get("note") else "")
    )
    lines.append("")

    kind, _, arg = stage.partition(":")
    if kind == "queued":
        lines.append(f"⏳ キュー待ち（前に {arg or '?'} 件）")
    elif kind == "deploying":
        lines.append("🚚 デプロイ中")
    elif kind == "benchmarking":
        lines.append(f"🏃 ベンチ実行中 [#{arg}](https://portal.isucon.net/contestant/benchmark_jobs/{arg})")
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
        hist = [h for h in history() if h.get("job_id") != job["id"]]
        base = baseline(hist)
        prev = hist[-1] if hist else None
        jid = job["id"]
        lines += [
            "",
            f"## {'✅ PASS' if ok else '❌ FAIL'} **{score}**"
            + (f"　vs main {delta(score, base)}" if ok and base else ""),
            "",
            "| | job | score | app | container |",
            "|---|---|---:|---|---|",
            f"| **this** | [#{jid}](https://portal.isucon.net/contestant/benchmark_jobs/{jid}) | {score} | "
            f"{sha_link(meta.get('app_repo'), meta.get('app_sha'))} | "
            f"{sha_link(meta.get('container_repo'), meta.get('container_sha'))} |",
        ]
        for name, h in (("baseline (main)", base), ("直前の CI ベンチ", prev)):
            if h:
                lines.append(
                    f"| {name}: {h.get('label')} | [#{h['job_id']}](https://portal.isucon.net/contestant/benchmark_jobs/{h['job_id']}) | "
                    f"{h['score']}{'' if h.get('passed') else ' (FAIL)'} | "
                    f"{sha_link(h.get('app_repo'), h.get('app_sha'))} | "
                    f"{sha_link(h.get('container_repo'), h.get('container_sha'))} |"
                )
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
        if days:
            lines += ["", "| region | 1 日目→最終日の人数 |", "|---|---|"]
            lines += [f"| {r} | {' → '.join(v)} |" for r, v in days.items()]
        gp = re.search(r'msg="(グランプリ: [^"]+)"', res.get("stdout", ""))
        if gp:
            lines.append(f"\n{gp.group(1)}")

    if integrity:
        lines += ["", "> [!WARNING]", "> ベンチ中にサーバの状態が変わりました。この結果は信用できません。"]
        lines += [f"> `{l}`" for l in integrity.splitlines()]
    if foreign:
        lines += ["", "> [!WARNING]",
                  f"> CI 以外から投げられたベンチがあります: {foreign}。サーバの状態が CI の想定と違う可能性があります。"]

    if job and (job.get("result") or {}).get("stdout"):
        lines += ["", "<details><summary>benchmarker stdout</summary>", "", "```",
                  job["result"]["stdout"][-6000:], "```", "</details>"]
    lines.append(f"\n<sub>{datetime.now(JST):%H:%M:%S} JST</sub>")
    return "\n".join(lines) + "\n"


def record():
    res = job.get("result") or {}
    return json.dumps({
        "job_id": job["id"],
        "score": job.get("score", 0),
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
    }, ensure_ascii=False)


def status():
    """commit status（context: Benchmark）の state と description。description は 140 字まで。"""
    kind, _, arg = stage.partition(":")
    if kind == "queued":
        state, desc = "pending", f"キュー待ち（前に {arg or '?'} 件）"
    elif kind == "deploying":
        state, desc = "pending", "デプロイ中"
    elif kind == "benchmarking":
        state, desc = "pending", f"ベンチ実行中 #{arg}"
    elif kind == "rejected":
        state, desc = "failure", arg
    elif kind == "done" and job:
        res = job.get("result") or {}
        score = job.get("score", 0)
        if res.get("passed"):
            base = baseline([h for h in history() if h.get("job_id") != job["id"]])
            state, desc = "success", f"PASS {score}" + (f"（main 比 {delta(score, base)}）" if base else "")
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
