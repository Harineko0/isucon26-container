#!/usr/bin/env python3
"""report.py / replica_job.py の単体テスト。全ケースが通れば exit 0。

  python3 ci/bench/report_test.py

公式ベンチ（ポータルのジョブ）と自作ベンチ（replica.sh の出力）の両方の run について、
コメント・results.jsonl の 1 行・commit status を作り、比較が同じ種類の run どうしで行われること、
同じ commit の組があれば較正（公式/自作 の比）が出ることを確かめる。
"""
import json
import os
import subprocess
import sys
import tempfile

B = os.path.dirname(os.path.abspath(__file__))
fails = 0


def check(name, cond, info=""):
    global fails
    print(("ok   " if cond else "FAIL ") + name + ("" if cond else f"  {info}"))
    fails += not cond


def run(script, arg, state, history):
    env = {**os.environ, "STATE_DIR": state, "HISTORY": history, "GITHUB_RUN_ID": "9001", "STACK": "isucon-replica-1"}
    r = subprocess.run([sys.executable, os.path.join(B, script), arg], capture_output=True, text=True, env=env)
    if r.returncode != 0:
        print(r.stderr)
    return r.stdout


def write(state, name, text):
    os.makedirs(os.path.dirname(os.path.join(state, name)), exist_ok=True)
    with open(os.path.join(state, name), "w") as f:
        f.write(text)


SHA = "e48943f230a8db2d6456993f7cf23013a250cd9b"
HIST = [
    {"job_id": 39800, "bench": "official", "score": 200000, "passed": True, "label": "main", "app_sha": "a" * 40,
     "finished_at": "2026-10-31T17:00:00+09:00", "run_url": "https://x/runs/1"},
    {"job_id": 8000, "bench": "isureal-bench", "stack": "isucon-replica-1", "score": 50000, "passed": True, "label": "main",
     "app_sha": "a" * 40, "finished_at": "2026-10-31T17:10:00+09:00", "run_url": "https://x/runs/8000"},
    {"job_id": 8100, "bench": "isureal-bench", "stack": "isucon-replica-2", "score": 52000, "passed": True, "label": "#322",
     "pr_number": "322", "pr_repo": "Harineko0/isucon26-container",
     "app_sha": "b" * 40, "finished_at": "2026-10-31T17:20:00+09:00", "run_url": "https://x/runs/8100"},
    {"job_id": 39824, "bench": "official", "score": 210244, "passed": True, "label": "#322", "pr_number": "322",
     "pr_repo": "Harineko0/isucon26-container", "app_sha": SHA, "finished_at": "2026-10-31T17:30:00+09:00", "run_url": "https://x/runs/2"},
]

with tempfile.TemporaryDirectory() as tmp:
    hist = os.path.join(tmp, "results.jsonl")
    with open(hist, "w") as f:
        f.write("\n".join(json.dumps(h) for h in HIST) + "\n")

    # --- 自作ベンチの run（PR #322、SHA は公式 #39824 と同じ） ---
    st = os.path.join(tmp, "replica")
    out = os.path.join(st, "replica")
    write(st, "meta.json", json.dumps({"label": "#322", "pr_repo": "Harineko0/isucon26-container", "pr_number": "322",
                                       "app_repo": "Harineko0/isucon26-container", "app_sha": SHA, "container_repo": "Harineko0/isucon26-container",
                                       "container_sha": SHA, "run_url": "https://x/runs/9001", "note": "", "actor": "t", "bench": "isureal-bench"}))
    write(st, "stack", "isucon-replica-1")
    write(st, "stage", "benchmarking:isucon-replica-1")
    md = run("report.py", "render", st, hist)
    check("replica: 実行中の表示", "自作ベンチ実行中（複製スタック isucon-replica-1）" in md, md)
    write(st, "stage", "done")
    write(out, "bench.log", 'time=2026-10-31T08:00:00.000Z level=INFO msg="asia 1 日目: 18 人"\n'
                            'time=2026-10-31T08:00:09.000Z level=INFO msg="asia 2 日目: 40 人"\n'
                            'time=2026-10-31T08:01:00.000Z level=INFO msg="グランプリ: アジア 100 人, ヨーロッパ 30 人, アメリカ 20 人"\n'
                            '{\n  "pass": true,\n  "score": 70000,\n  "messages": []\n}\n')
    write(out, "exit_status", "0\n")
    write(out, "started_at", "2026-10-31T08:00:00Z\n")
    write(out, "finished_at", "2026-10-31T08:02:00Z\n")
    write(out, "result.json", json.dumps({
        "pass": True, "score": 70000, "messages": [], "rawScore": 70000,
        "breakdown": [{"kind": "post_image", "count": 1200, "points": 2400}, {"kind": "friend_feed_pass", "count": 300, "points": 3000}],
        "byRegion": {"asia": 30000, "europe": 25000, "america": 15000},
        "requests": [{"label": "GET /api/feeds/friends", "count": 5000, "errors": 1, "p50_ms": 12, "p95_ms": 80, "max_ms": 900}],
        "totalRequests": 60000, "errorCount": 3, "errorRate": 0.00005, "initializeSec": 2.5, "language": "go", "loadSec": 63,
        "daysUsers": {"asia": [18, 40, 90], "europe": [7, 15, 30], "america": [6, 12, 20]},
        "daysPassed": {"asia": [10, 30, 70], "europe": [5, 10, 20], "america": [4, 8, 15]},
        "grandPrixPassed": {"asia": 80, "europe": 20, "america": 10},
        "users": 300, "posts": 1500, "lateFeedVisibility": 2, "staleCaption": 0, "staleProfile": 0}))
    job = run("replica_job.py", out, st, hist)
    write(st, "job.json", job)
    j = json.loads(job)
    check("replica_job: FINISHED/PASS/score", j["status"] == "FINISHED" and j["result"]["passed"] and j["score"] == 70000, job[:200])
    check("replica_job: id は run_id、bench/stack", j["id"] == 9001 and j["bench"] == "isureal-bench" and j["stack"] == "isucon-replica-1")
    md = run("report.py", "render", st, hist)
    check("replica: 見出しに自作ベンチ", "自作ベンチ（複製スタック isucon-replica-1）" in md)
    check("replica: 前回比は同じ PR の自作ベンチ #8100 と", "前回比 +18000 (+34.6%)" in md, md)
    check("replica: 比較表の前回は run 8100（公式 #39824 ではない）", "[run 8100](https://x/runs/8100)" in md and "benchmark_jobs/39824 | 210244" not in md.split("較正")[0])
    check("replica: baseline は自作ベンチの main（run 8000）", "baseline (main): main | [run 8000]" in md, md)
    check("replica: 較正に同じ commit の公式 210244", "同じ commit の公式ベンチ **210244**" in md and "公式/自作 = **3.003**" in md, md)
    check("replica: 公式換算の目安（中央値）", "公式換算の目安" in md, md)
    check("replica: 内訳の details", "自作ベンチの内訳" in md and "| friend_feed_pass | 300 | 3,000 |" in md, md)
    check("replica: read-after-write の警告", "⚠️ read-after-write の疑い" in md and "再試行 2" in md, md)
    check("replica: グランプリの行", "グランプリ: アジア 100 人" in md)
    rec = json.loads(run("report.py", "record", st, hist))
    check("replica record: bench/stack/pair", rec["bench"] == "isureal-bench" and rec["stack"] == "isucon-replica-1"
          and rec["pair_job_id"] == 39824 and rec["pair_bench"] == "official" and abs(rec["pair_ratio"] - 3.0035) < 0.001
          and rec["late_feed"] == 2, rec)
    stt = json.loads(run("report.py", "status", st, hist))
    check("replica status: success と前回比", stt["state"] == "success" and "前回比" in stt["description"], stt)

    # --- 自作ベンチが結果を書かずに終わった ---
    st2 = os.path.join(tmp, "replica-fail")
    out2 = os.path.join(st2, "replica")
    write(out2, "bench.log", "replica.sh: ...\n")
    write(out2, "exit_status", "124\n")
    j2 = json.loads(run("replica_job.py", out2, st2, hist))
    check("replica_job: timeout → ERRORED/FAIL", j2["status"] == "ERRORED" and not j2["result"]["passed"] and "タイムアウト" in j2["result"]["reason"], j2)

    # --- 公式ベンチの run（main、SHA は自作 run 8000 と同じ a*40） ---
    st3 = os.path.join(tmp, "official")
    write(st3, "meta.json", json.dumps({"label": "main", "pr_repo": "Harineko0/isucon26-container", "pr_number": "",
                                        "app_repo": "Harineko0/isucon26-container", "app_sha": "a" * 40, "container_repo": "Harineko0/isucon26-container",
                                        "container_sha": "a" * 40, "run_url": "https://x/runs/9002", "note": "", "actor": "t", "bench": "official"}))
    write(st3, "stage", "done")
    write(st3, "job.json", json.dumps({"id": 39900, "status": "FINISHED", "score": 205000, "finished_at": 1761900000,
                                       "result": {"passed": True, "reason": "", "stdout": 'msg="asia 1 日目: 18 人"\n', "messages": []}}))
    md3 = run("report.py", "render", st3, hist)
    check("official: 見出しに公式ベンチ", "公式ベンチ（本番）" in md3)
    check("official: main 比は公式の main #39800 と", "main 比 +5000 (+2.5%)" in md3, md3)
    check("official: this はポータルへのリンク", "benchmark_jobs/39900" in md3)
    check("official: 直前の CI ベンチは公式 #39824（自作 run は混ざらない）", "直前の CI ベンチ: #322 | [#39824]" in md3, md3)
    check("official: 較正に同じ commit の自作 50000", "同じ commit の自作ベンチ **50000**" in md3 and "公式/自作 = **4.100**" in md3, md3)
    rec3 = json.loads(run("report.py", "record", st3, hist))
    check("official record: bench=official, stack=isucon-prod-2, pair=run 8000", rec3["bench"] == "official" and rec3["stack"] == "isucon-prod-2"
          and rec3["pair_job_id"] == 8000 and abs(rec3["pair_ratio"] - 4.1) < 0.001, rec3)

    # --- 古い results.jsonl（bench 無し）は official として扱う ---
    with open(hist, "w") as f:
        f.write(json.dumps({"job_id": 1, "score": 100, "passed": True, "label": "main", "app_sha": "c" * 40}) + "\n")
    md4 = run("report.py", "render", st3, hist)
    check("official: bench 無しの記録は公式として比較", "main 比 +204900" in md4, md4)
    md5 = run("report.py", "render", st, hist)
    check("replica: bench 無しの記録とは比較しない", "main 比" not in md5 and "前回比" not in md5, md5)

print("all passed" if not fails else f"{fails} failed")
sys.exit(1 if fails else 0)
