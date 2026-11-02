#!/usr/bin/env python3
"""自作ベンチ（replica.sh）の出力を、ポータルのジョブ（portal.py show）と同じ形の job.json にする。

  replica_job.py <out-dir>   # stdout に JSON

report.py / finalize.sh はポータルのジョブと同じ鍵（id, status, score, finished_at, result.{passed, reason, stdout, messages}）
を読むので、公式ベンチと同じ経路で記録・コメントできる。違いは bench="isureal-bench" と stack、result.detail（内訳）。
env: GITHUB_RUN_ID（id に使う）, STACK
"""
import json
import os
import sys
from datetime import datetime, timezone


def read(path, default=None):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            return f.read()
    except FileNotFoundError:
        return default


def epoch(s):
    if not s:
        return None
    try:
        return datetime.strptime(s.strip(), "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp()
    except ValueError:
        return None


def main(out):
    log = read(os.path.join(out, "bench.log"), "") or ""
    status = (read(os.path.join(out, "exit_status"), "") or "").strip()
    raw = read(os.path.join(out, "result.json"))
    res = None
    if raw:
        try:
            res = json.loads(raw)
        except ValueError:
            res = None
    run_id = int(os.environ.get("GITHUB_RUN_ID") or 0)
    job = {
        "id": run_id,
        "bench": "isureal-bench",
        "stack": os.environ.get("STACK", ""),
        "target_id": None,
        "status": "FINISHED" if res is not None else "ERRORED",
        "created_at": epoch(read(os.path.join(out, "started_at"))),
        "started_at": epoch(read(os.path.join(out, "started_at"))),
        "finished_at": epoch(read(os.path.join(out, "finished_at"))) or datetime.now(timezone.utc).timestamp(),
    }
    if res is None:
        reason = "isureal-bench が結果を書かずに終わりました"
        if status == "124":
            reason = "isureal-bench がタイムアウトしました（BENCH_TIMEOUT）"
        elif status == "255":
            reason = "ベンチ実行元（BENCH_RUNNER）に入れませんでした"
        elif status == "2":
            reason = "isureal-bench をビルドできませんでした"
        elif status:
            reason += f"（exit {status}）"
        job["score"] = 0
        job["result"] = {"finished": True, "passed": False, "score": 0, "reason": reason, "stdout": log[-6000:], "messages": [reason]}
    else:
        passed = bool(res.get("pass"))
        score = int(res.get("score", 0)) if passed else 0
        job["score"] = score
        job["result"] = {
            "finished": True,
            "passed": passed,
            "score": score,
            "raw": int(res.get("rawScore", 0)),
            "deduction": 0,
            "reason": res.get("reason", "") if not passed else "",
            "stdout": log[-6000:],
            "messages": list(res.get("messages") or [])[:50],
            # 自作ベンチだけの内訳（report.py が表にする）
            "detail": {
                "breakdown": res.get("breakdown") or [],
                "byRegion": res.get("byRegion") or {},
                "requests": sorted(res.get("requests") or [], key=lambda r: -(r.get("count") or 0))[:25],
                "totalRequests": res.get("totalRequests"),
                "errorCount": res.get("errorCount"),
                "errorRate": res.get("errorRate"),
                "initializeSec": res.get("initializeSec"),
                "loadSec": res.get("loadSec"),
                "language": res.get("language"),
                "users": res.get("users"),
                "posts": res.get("posts"),
                "daysUsers": res.get("daysUsers") or {},
                "daysPassed": res.get("daysPassed") or {},
                "grandPrixPassed": res.get("grandPrixPassed") or {},
                "lateFeedVisibility": res.get("lateFeedVisibility"),
                "staleCaption": res.get("staleCaption"),
                "staleProfile": res.get("staleProfile"),
            },
        }
    json.dump(job, sys.stdout, ensure_ascii=False)
    print()


if __name__ == "__main__":
    main(sys.argv[1])
