#!/bin/bash
# bench ワークフローの自分より前の run（run_number が小さいもの）が全部終わるまで待つ（FIFO）。
# GitHub Actions の concurrency は待ちを 1 本しか保持せず古い待ちを捨てるので使わない。
#
#   GH_TOKEN, GITHUB_REPOSITORY, GITHUB_RUN_NUMBER, WORKFLOW_FILE（既定 bench.yml）
#   ON_WAIT: 待ち順位が変わるたびに "$ON_WAIT <ahead>" を実行する（PR コメントの更新用）
#   BENCH_KIND: official なら、公式ベンチ（本番スタック + ポータル）の run だけを数える。複製スタックで走る自作ベンチの
#   run（run-name が " replica" で終わる。bench.yml の run-name）は本番に触らないので待たない。
#   replica の run はこの FIFO に入らない（stacklock.sh で空いているスタックを取る）。
set -eu
WF=${WORKFLOW_FILE:-bench.yml}
me=$GITHUB_RUN_NUMBER
prev=
filter=""
[ "${BENCH_KIND:-official}" != official ] || filter=' and ((.display_title // "") | endswith(" replica") | not)'

while :; do
  ahead=$(gh api "repos/$GITHUB_REPOSITORY/actions/workflows/$WF/runs?per_page=100" \
    --jq "[.workflow_runs[] | select(.status != \"completed\" and .run_number < $me$filter)] | length")
  [ "$ahead" = 0 ] && break
  if [ "$ahead" != "$prev" ]; then
    echo "waiting: $ahead run(s) ahead"
    [ -z "${ON_WAIT:-}" ] || $ON_WAIT "$ahead" || true
    prev=$ahead
  fi
  sleep 15
done
echo "queue: my turn (run #$me)"
