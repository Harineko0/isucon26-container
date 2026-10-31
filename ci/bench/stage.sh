#!/bin/bash
# 進行状況を記録して PR コメントと commit status を書き換える。
#   stage.sh <stage> [arg]   例: stage.sh queued 2 / stage.sh deploying / stage.sh benchmarking 1234
set -u
B=$(cd "$(dirname "$0")" && pwd)
echo "$1${2:+:$2}" >"$STATE_DIR/stage"
python3 "$B/report.py" render >"$STATE_DIR/comment.md"
"$B/comment.sh" "$STATE_DIR/comment.md" || echo "warn: comment update failed"
"$B/status.sh" || echo "warn: status update failed"
