#!/bin/bash
# 自作ベンチ（bench/cmd/isureal-bench）を複製スタックのルーターに向けて走らせる。CI（run.sh）から呼ぶ。
#
#   replica.sh <router-url> <out-dir>
#
# env:
#   BENCH_RUNNER      ベンチを動かす東京リージョンの EC2（user@host）。空なら GitHub の runner で動かす
#                     （往復 150 ms が乗るので絶対値は下がる。較正には使わない）
#   BENCH_RUNNER_KEY  その鍵（既定 ~/.ssh/id_ed25519_github = DEPLOY_SSH_KEY）
#   BENCH_FLAGS       isureal-bench に足すフラグ（例 "-days 5 -seed 1"）。-manifest は付けない（本番の初期データの
#                     SecretKey が分からないので、利用者を登録して走る load-only。bench/README.md）
#   BENCH_TIMEOUT     ベンチ全体の打ち切り秒（既定 600。公式は約 2 分）
# 出力: <out-dir>/bench.log（stdout。公式と同じ形のログ + 末尾の JSON）、<out-dir>/result.json（内訳）、
#       <out-dir>/exit_status（isureal-bench の終了コード。124 = timeout）。この script 自身は常に exit 0
#       （結果の判定は replica_job.py が job.json にする）。
set -uo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
router=${1:?usage: replica.sh <router-url> <out-dir>}
out=${2:?usage: replica.sh <router-url> <out-dir>}
mkdir -p "$out"
RUNNER=${BENCH_RUNNER:-}
KEY=${BENCH_RUNNER_KEY:-$HOME/.ssh/id_ed25519_github}
TIMEOUT=${BENCH_TIMEOUT:-600}

# 静的バイナリ。EC2（amd64 の Linux）に送るので EC2 側に Go は要らない。BENCH_RUNNER が無ければこのマシン向けに作る
if [ -n "$RUNNER" ]; then export CGO_ENABLED=0 GOOS=linux GOARCH=amd64; fi
if ! (cd "$ROOT/bench" && go build -trimpath -o "$out/isureal-bench" ./cmd/isureal-bench); then
  echo "replica.sh: isureal-bench のビルドに失敗" | tee "$out/bench.log"
  echo 2 >"$out/exit_status"
  exit 0
fi

flags=(-target "$router" -out result.json)
# shellcheck disable=SC2206
extra=(${BENCH_FLAGS:-})
status=0
date -u +%FT%TZ >"$out/started_at"
if [ -z "$RUNNER" ]; then
  echo "replica.sh: BENCH_RUNNER が無いので GitHub の runner で走らせる（往復 150 ms が乗る）"
  (cd "$out" && timeout "$TIMEOUT" ./isureal-bench "${flags[@]}" "${extra[@]}" | tee bench.log) || status=$?
else
  dir="isureal-bench/${GITHUB_RUN_ID:-local}-$$"
  SSH=(ssh -i "$KEY" -o ConnectTimeout=15 -o BatchMode=yes -o ServerAliveInterval=30 -o StrictHostKeyChecking=accept-new "$RUNNER")
  if ! "${SSH[@]}" "mkdir -p $dir" || ! scp -q -i "$KEY" -o BatchMode=yes -o StrictHostKeyChecking=accept-new "$out/isureal-bench" "$RUNNER:$dir/"; then
    echo "replica.sh: ベンチ実行元 $RUNNER に入れない（BENCH_RUNNER と DEPLOY_SSH_KEY を確かめる）" | tee "$out/bench.log"
    echo 255 >"$out/exit_status"
    exit 0
  fi
  echo "replica.sh: running on $RUNNER: isureal-bench ${flags[*]} ${extra[*]}"
  "${SSH[@]}" "cd $dir && timeout $TIMEOUT ./isureal-bench $(printf '%q ' "${flags[@]}" "${extra[@]}")" | tee "$out/bench.log" || status=$?
  scp -q -i "$KEY" -o BatchMode=yes "$RUNNER:$dir/result.json" "$out/result.json" 2>/dev/null || echo "replica.sh: result.json を取れなかった（ベンチが途中で終わった）"
  "${SSH[@]}" "rm -rf $dir" 2>/dev/null || true
fi
date -u +%FT%TZ >"$out/finished_at"
echo "$status" >"$out/exit_status"
echo "replica.sh: isureal-bench exit $status"
exit 0
