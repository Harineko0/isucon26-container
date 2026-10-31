#!/bin/bash
# bench-control のフラグを毎回取得する。取得失敗・不正値では進めない。
set -euo pipefail
while :; do
  control=$(gh api "repos/$GITHUB_REPOSITORY/contents/bench-control.json?ref=bench-control" -H 'Accept: application/vnd.github.raw+json')
  jq -e '(.enabled | type == "boolean") and (.allowed_run_id | type == "string")' <<<"$control" >/dev/null
  if [ "$(jq -r .enabled <<<"$control")" = true ]; then
    rm -f "$STATE_DIR/exclusive"
    exit 0
  fi
  if [ "$(jq -r .allowed_run_id <<<"$control")" = "$GITHUB_RUN_ID" ]; then
    touch "$STATE_DIR/exclusive"
    exit 0
  fi
  echo 'bench OFF: waiting for ON (or explicit run authorization)'
  sleep 15
done
