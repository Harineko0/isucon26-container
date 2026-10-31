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

# キュー待ち・ポータル待ちの間に凍結時刻を過ぎていたら、サーバに触らずに拒否する（#245）。
# `||` で受けるので ERR trap（failed:*）は走らず、stage は freeze.sh が書いた rejected:* のまま
"$B/freeze.sh" || exit 1

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
  out=$(curl -s -o "$STATE_DIR/init.json" -w '%{http_code} %{time_total}' -m 60 -XPOST "$ROUTER/api/initialize" || true)
  code=${out%% *}
  [ "$code" = 200 ] && break
  echo "initialize: HTTP $out (retry $i)"
  sleep 5
done
[ "$code" = 200 ] || { cat "$STATE_DIR/init.json"; false; }
cat "$STATE_DIR/init.json"; echo
# デプロイ直後（app 再起動直後、002〜005 を流す最初の回）の所要時間。索引追加の目安に使う（ci/README.md #40）
echo "${out#* }" >"$STATE_DIR/init_time"
echo "initialize: $(cat "$STATE_DIR/init_time")s"

step=fingerprint
"$B/fingerprint.sh" | tee "$STATE_DIR/fp-before.txt"
# app 3 台が、この run の app commit で動いていること（指紋の 2 列目 = DEPLOYED_SHA）
n=$(awk -v sha="${APP_SHA:0:12}" '$2 == sha' "$STATE_DIR/fp-before.txt" | wc -l)
[ "$n" -eq 3 ] || { echo "DEPLOYED_SHA: $n/3 hosts run app@${APP_SHA:0:12}"; false; }

step=enqueue
wait_portal_idle
# 計測（#37）: 統計とアクセスログをこの run の版だけにしてから、ベンチ中の vmstat / DB wait を記録する。
# 計測の失敗ではベンチを止めない（measure.sh は常に exit 0。念のため || true）。
M=$STATE_DIR/measure
bash "$B/measure.sh" reset || true
bash "$B/measure.sh" start "$M" || true
job_id=$(python3 "$B/portal.py" enqueue)
echo "$job_id" >"$STATE_DIR/job_id"
echo "enqueued job #$job_id"
"$B/stage.sh" benchmarking "$job_id"

step=benchmark
python3 "$B/portal.py" wait "$job_id" >"$STATE_DIR/job.json"
jq '{id, status, score, result: (.result | {passed, reason, messages})}' "$STATE_DIR/job.json"
bash "$B/measure.sh" stop "$M" || true

step=integrity
"$B/fingerprint.sh" >"$STATE_DIR/fp-after.txt"
{
  diff "$STATE_DIR/fp-before.txt" "$STATE_DIR/fp-after.txt" | grep '^[<>]' || true
  infra/deploy.sh -n | grep -v 'no changes$' | sed 's/^/config drift: /' || true
} >"$STATE_DIR/integrity.txt"
cat "$STATE_DIR/integrity.txt"

step=measure
timeout 240 bash "$B/measure.sh" collect "$M" || true

echo done >"$STATE_DIR/stage"
