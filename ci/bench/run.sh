#!/bin/bash
# キューの順番が来たあとに CI から呼ぶ: デプロイ → 確認 → ベンチ → 改変チェック。
# 結果の記録と最終コメントは finalize.sh（失敗しても走る）がやる。
# env: STATE_DIR, HISTORY, PORTAL_COOKIE, ほか comment.sh の env
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
B=$ROOT/ci/bench
ROUTER=${ROUTER:-http://35.73.68.236:8080}
cd "$ROOT"

step=init
trap 'echo "failed:$step" >"$STATE_DIR/stage"' ERR

wait_portal_idle() {
  local deadline=$((SECONDS + 900)) active
  while :; do
    active=$(python3 "$B/portal.py" active | paste -sd' ' -)
    [ -n "$active" ] || return 0
    [ $SECONDS -lt $deadline ] || { echo "portal job(s) still active: $active"; return 1; }
    echo "portal busy: $active"
    sleep 10
  done
}

step=portal-idle
wait_portal_idle

# CI の記録にない portal ジョブ = 誰かが手で投げたベンチ
step=foreign-check
last_ci=$(tail -n1 "$HISTORY" 2>/dev/null | jq -r '.job_id // 0' || echo 0)
python3 "$B/portal.py" list 50 | jq -r --argjson last "${last_ci:-0}" 'select(.id > $last) | "#\(.id)"' |
  paste -sd' ' - >"$STATE_DIR/foreign.txt"
[ "${last_ci:-0}" != 0 ] || : >"$STATE_DIR/foreign.txt" # 記録が空のうちは判定しない

step=deploy-app
"$B/stage.sh" deploying
./deploy.sh 2>&1 | tee "$STATE_DIR/deploy-app.log"
n=$(grep -c ' deployed$' "$STATE_DIR/deploy-app.log" || true)
[ "$n" = 3 ] || { echo "deploy.sh: $n/3 hosts deployed"; false; }

step=deploy-infra
infra/deploy.sh 2>&1 | tee "$STATE_DIR/deploy-infra.log"
if grep -E '\] ERROR|"passed": false' "$STATE_DIR/deploy-infra.log"; then false; fi

step=initialize
for i in 1 2 3 4 5 6; do
  code=$(curl -s -o "$STATE_DIR/init.json" -w '%{http_code}' -m 60 -XPOST "$ROUTER/api/initialize" || true)
  [ "$code" = 200 ] && break
  echo "initialize: HTTP $code (retry $i)"
  sleep 5
done
[ "$code" = 200 ] || { cat "$STATE_DIR/init.json"; false; }
cat "$STATE_DIR/init.json"; echo

step=fingerprint
"$B/fingerprint.sh" | tee "$STATE_DIR/fp-before.txt"

step=enqueue
wait_portal_idle
job_id=$(python3 "$B/portal.py" enqueue)
echo "$job_id" >"$STATE_DIR/job_id"
echo "enqueued job #$job_id"
"$B/stage.sh" benchmarking "$job_id"

step=benchmark
python3 "$B/portal.py" wait "$job_id" >"$STATE_DIR/job.json"
jq '{id, status, score, result: (.result | {passed, reason, messages})}' "$STATE_DIR/job.json"

step=integrity
"$B/fingerprint.sh" >"$STATE_DIR/fp-after.txt"
{
  diff "$STATE_DIR/fp-before.txt" "$STATE_DIR/fp-after.txt" | grep '^[<>]' || true
  infra/deploy.sh -n | grep -v 'no changes$' | sed 's/^/config drift: /' || true
} >"$STATE_DIR/integrity.txt"
cat "$STATE_DIR/integrity.txt"

echo done >"$STATE_DIR/stage"
