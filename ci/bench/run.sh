#!/bin/bash
# キューの順番が来たあとに CI から呼ぶ: デプロイ → 確認 → ベンチ → 改変チェック。
# 結果の記録と最終コメントは finalize.sh（失敗しても走る）がやる。
#
# BENCH_KIND=official（既定）: 本番スタック（isucon-prod-2）にデプロイし、ポータルの公式ベンチを投げる（main、workflow_dispatch）。
# BENCH_KIND=replica:          STACK（stacklock.sh が取った複製スタック）にデプロイし、自作ベンチ（replica.sh、isureal-bench）を
#                              東京の EC2 から走らせる（PR の /bench）。ポータルには触らない。
# env: STATE_DIR, HISTORY, STACK（replica）, PORTAL_COOKIE（official）, BENCH_RUNNER / BENCH_FLAGS（replica）, ほか comment.sh の env
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
B=$ROOT/ci/bench
BENCH_KIND=${BENCH_KIND:-official}
case $BENCH_KIND in
  official) export STACK=${STACK:-isucon-prod-2} ;;
  replica) [ -n "${STACK:-}" ] || { echo "run.sh: BENCH_KIND=replica には STACK（複製スタック名）が要る" >&2; exit 1; } ;;
  *) echo "run.sh: BENCH_KIND=$BENCH_KIND ?" >&2; exit 1 ;;
esac
. "$ROOT/ci/stacks/lib.sh" # STACK → ASIA1_IP … ROUTER_URL（infra/lib.sh、deploy.sh も同じものを読む）
ROUTER=${ROUTER:-$ROUTER_URL}
if [ "$BENCH_KIND" = official ] && [ "$STACK_KIND" != prod ]; then
  echo "run.sh: 公式ベンチは本番スタックにしか向けない（STACK=$STACK は $STACK_KIND）" >&2
  exit 1
fi
if [ "$BENCH_KIND" = replica ] && [ "$STACK_KIND" = prod ]; then
  echo "run.sh: 自作ベンチを本番スタックには向けない（STACK=$STACK）。複製スタックを BENCH_REPLICA_STACKS に入れる" >&2
  exit 1
fi
echo "run.sh: $BENCH_KIND bench on stack $STACK_NAME ($STACK_KIND): app $APP_IPS / db $ASIA2_IP / router $ROUTER"
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

if [ "$BENCH_KIND" = official ]; then
  step=portal-idle
  wait_portal_idle
fi

# キュー待ち・ポータル待ちの間に凍結時刻を過ぎていたら、サーバに触らずに拒否する（#245）。
# `||` で受けるので ERR trap（failed:*）は走らず、stage は freeze.sh が書いた rejected:* のまま
"${FREEZE_CHECK:-$B/freeze.sh}" || exit 1

# CI の記録にない portal ジョブ = 誰かが手で投げたベンチ
"${BENCH_GATE:-$B/gate.sh}"

: >"$STATE_DIR/foreign.txt"
if [ "$BENCH_KIND" = official ]; then
  step=foreign-check
  last_ci=$(jq -r 'select((.bench // "official") == "official") | .job_id' "$HISTORY" 2>/dev/null | tail -n1 || echo 0)
  python3 "$B/portal.py" list 50 | jq -r --argjson last "${last_ci:-0}" 'select(.id > $last) | "#\(.id)"' |
    paste -sd' ' - >"$STATE_DIR/foreign.txt"
  [ "${last_ci:-0}" != 0 ] || : >"$STATE_DIR/foreign.txt" # 記録が空のうちは判定しない
fi

# A previous failed infra deployment may have left PgBouncer inaccessible.
# Repair it before deploy.sh restarts the app using the already-installed unit;
# otherwise deploy-app fails before infra/deploy.sh gets a chance to repair it.
step=pool-permissions
ssh -i ~/.ssh/id_ed25519_github -o StrictHostKeyChecking=accept-new "isucon@$ASIA1_IP" \
  'if sudo test -f /usr/local/lib/isureal/pgbouncer-1.26.1; then bash -s -- 0; fi' \
  <"$ROOT/infra/pgbouncer/permissions.sh"

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

# 計測（#37）: 統計とアクセスログをこの run の版だけにしてから、ベンチ中の vmstat / DB wait を記録する。
# 計測の失敗ではベンチを止めない（measure.sh は常に exit 0。念のため || true）。
M=$STATE_DIR/measure
if [ "$BENCH_KIND" = official ]; then
  step=enqueue
  wait_portal_idle
  bash "$B/measure.sh" reset || true
  bash "$B/measure.sh" start "$M" || true
  job_id=$(python3 "$B/portal.py" enqueue)
  echo "$job_id" >"$STATE_DIR/job_id"
  echo "enqueued job #$job_id"
  "$B/stage.sh" benchmarking "$job_id"

  step=benchmark
  python3 "$B/portal.py" wait "$job_id" >"$STATE_DIR/job.json"
else
  step=benchmark
  bash "$B/measure.sh" reset || true
  bash "$B/measure.sh" start "$M" || true
  "$B/stage.sh" benchmarking "$STACK"
  # 自作ベンチ（load-only、-manifest 無し）。replica.sh は常に exit 0 で、結果は replica/ に残る
  "$B/replica.sh" "$ROUTER" "$STATE_DIR/replica"
  python3 "$B/replica_job.py" "$STATE_DIR/replica" >"$STATE_DIR/job.json"
fi
jq '{id, bench, stack, status, score, result: (.result | {passed, reason, messages: (.messages[:10])})}' "$STATE_DIR/job.json"
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
