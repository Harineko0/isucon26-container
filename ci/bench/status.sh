#!/bin/bash
# ベンチした commit（container 側）に commit status「Benchmark」を付ける。
# PR の status checks に出るので、/bench で測り直した結果も最新の commit に残る。
# env: STATE_DIR, COMMENT_TOKEN, GITHUB_REPOSITORY
set -u
B=$(cd "$(dirname "$0")" && pwd)
sha=$(jq -r '.container_sha // empty' "$STATE_DIR/meta.json")
[ -n "$sha" ] || exit 0
# 公式ベンチは「Benchmark」、複製スタックの自作ベンチは「Benchmark (isureal-bench)」。同じ PR に両方あっても区別できる
context=Benchmark
[ "$(jq -r '.bench // "official"' "$STATE_DIR/meta.json")" = official ] || context="Benchmark (isureal-bench)"
python3 "$B/report.py" status |
  jq --arg url "$(jq -r .run_url "$STATE_DIR/meta.json")" --arg ctx "$context" '. + {context: $ctx, target_url: $url}' |
  GH_TOKEN=$COMMENT_TOKEN gh api -X POST "repos/$GITHUB_REPOSITORY/statuses/$sha" --input - >/dev/null
