#!/bin/bash
# bench ワークフローの自分より前の run（run_number が小さいもの）が全部終わるまで待つ（FIFO）。
# GitHub Actions の concurrency は待ちを 1 本しか保持せず古い待ちを捨てるので使わない。
#
#   GH_TOKEN, GITHUB_REPOSITORY, GITHUB_RUN_NUMBER, WORKFLOW_FILE（既定 bench.yml）
#   ON_WAIT: 待ち順位が変わるたびに "$ON_WAIT <ahead>" を実行する（PR コメントの更新用）
set -eu
WF=${WORKFLOW_FILE:-bench.yml}
me=$GITHUB_RUN_NUMBER
prev=

while :; do
  ahead=$(gh api "repos/$GITHUB_REPOSITORY/actions/workflows/$WF/runs?per_page=100" \
    --jq "[.workflow_runs[] | select(.status != \"completed\" and .run_number < $me)] | length")
  [ "$ahead" = 0 ] && break
  if [ "$ahead" != "$prev" ]; then
    echo "waiting: $ahead run(s) ahead"
    [ -z "${ON_WAIT:-}" ] || $ON_WAIT "$ahead" || true
    prev=$ahead
  fi
  sleep 15
done
echo "queue: my turn (run #$me)"
