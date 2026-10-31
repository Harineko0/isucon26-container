#!/bin/bash
# 凍結時刻（FREEZE_AT）以降は main 以外を拒否する。拒否したら PR コメント・status を rejected にして exit 1。
# キュー待ち前（bench.yml）と、待ち明け・デプロイ直前（run.sh）の両方から呼ぶ（#245）。
# env: FREEZE_AT, LABEL, ほか stage.sh の env
set -u
B=$(cd "$(dirname "$0")" && pwd)
if [ "$(date +%s)" -ge "$(date -d "$FREEZE_AT" +%s)" ] && [ "$LABEL" != main ]; then
  "$B/stage.sh" rejected "$FREEZE_AT 以降は main（安定版）しかベンチしません（最終スコア = 最後のベンチ）"
  exit 1
fi
