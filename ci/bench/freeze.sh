#!/bin/bash
# 凍結時刻（FREEZE_AT）以降は main 以外を拒否する。拒否したら PR コメント・status を rejected にして exit 1。
# bench.yml がワークフローと同じ commit のこのファイルを $FREEZE_CHECK に置き、キュー待ちの前・後と
# run.sh のデプロイ直前から呼ぶ（#245）。stage.sh は作業ツリー（PR の head）のものを使う。
# env: FREEZE_AT, LABEL, ほか stage.sh の env
set -u
if [ "$(date +%s)" -ge "$(date -d "$FREEZE_AT" +%s)" ] && [ "$LABEL" != main ]; then
  "${GITHUB_WORKSPACE:-.}/ci/bench/stage.sh" rejected "$FREEZE_AT 以降は main（安定版）しかベンチしません（最終スコア = 最後のベンチ）"
  exit 1
fi
