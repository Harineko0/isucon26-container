#!/usr/bin/env bash
# ローカル環境 (docker compose) を立てて isureal-bench を 1 回走らせる。
#
#   bench/local/run.sh                      # 作業ツリーのアプリを small の初期データで測る
#   bench/local/run.sh -r origin/main       # 別の commit のアプリを測る (git archive した木をビルドする)
#   bench/local/run.sh -n a -i 1 -r <ref>   # 名前・サブネット・ポートを変えて別スタックを立てる (並列用)
#   bench/local/run.sh -- -days 3 -log-level debug   # -- 以降は isureal-bench にそのまま渡す
#
# オプション:
#   -n NAME        スタック名 (compose project = isureal-NAME、既定 bench)
#   -i INDEX       サブネット・ポートの番号 (0: 192.168.0.0/20 本番と同じ IP / N: 10.(200+N).0.0/20。既定 0)
#   -p PORT        ルーターのホスト側ポート (既定 28080+INDEX)
#   -r REF         測るアプリの git ref (既定: 作業ツリー)
#   --scale S      初期データの大きさ tiny / small / medium (既定 small)
#   --datagen      初期データを作り直す (既定: manifest.json が無いときだけ作る)
#   --no-build     イメージをビルドしない
#   --keep         終わってもコンテナを止めない
#   --down         スタックを止めてボリュームも消して終わる
#   --in-docker    ベンチをホストの go ではなくコンテナで動かす
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
name=bench index=0 ref="" scale=small keep=0 build=1 datagen=auto port="" down=0 in_docker=0
while [[ $# -gt 0 ]]; do
  case $1 in
    -n) name=$2; shift 2 ;;
    -i) index=$2; shift 2 ;;
    -p) port=$2; shift 2 ;;
    -r) ref=$2; shift 2 ;;
    --scale) scale=$2; shift 2 ;;
    --datagen) datagen=1; shift ;;
    --no-build) build=0; shift ;;
    --keep) keep=1; shift ;;
    --down) down=1; shift ;;
    --in-docker) in_docker=1; shift ;;
    --) shift; break ;;
    -h|--help) sed -n '2,24p' "$0"; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
done
bench_flags=("$@")

if (( index == 0 )); then
  export NET_PREFIX=192.168 SUBNET=192.168.0.0/20
else
  export NET_PREFIX="10.$((200 + index))" SUBNET="10.$((200 + index)).0.0/20"
fi
export COMPOSE_PROJECT_NAME="isureal-$name"
export ROUTER_PORT="${port:-$((28080 + index))}"
export DB_PORT="$((25432 + index))"
export WORK_DIR="$here/work/$name"
export APP_IMAGE="isureal-app:$name"
export TOOLS_IMAGE="isureal-bench-tools:local"
mkdir -p "$WORK_DIR"
compose=(docker compose -f "$here/docker-compose.yml")

if (( down )); then
  "${compose[@]}" down -v --remove-orphans
  exit 0
fi

case $scale in
  tiny) export DATAGEN_FLAGS="-users 150 -days 20 -seed-users asia=6,europe=3,america=3" ;;
  small) export DATAGEN_FLAGS="-users 600 -days 60" ;;
  medium) export DATAGEN_FLAGS="-users 2000 -days 120 -image-bytes 8192" ;;
  *) echo "unknown scale: $scale" >&2; exit 2 ;;
esac

# 測るアプリの木。-r なら git archive して別のビルドコンテキストにする
ctx="$repo"
if [[ -n $ref ]]; then
  ctx=$(mktemp -d "${TMPDIR:-/tmp}/isureal-ctx.XXXXXX")
  trap 'rm -rf "$ctx"' EXIT
  git -C "$repo" archive --format=tar "$ref" app/webapp/go app/webapp/sql app/webapp/public infra/app/etc/nginx | tar -x -C "$ctx"
  export NGINX_CONF="$ctx/infra/app/etc/nginx/sites-available/isureal.conf" PUBLIC_DIR="$ctx/app/webapp/public"
  echo "app: $ref ($(git -C "$repo" rev-parse --short "$ref"))"
else
  echo "app: working tree ($(git -C "$repo" rev-parse --short HEAD)$(git -C "$repo" diff --quiet -- app infra || echo ' +dirty'))"
fi
export APP_CONTEXT="$ctx" APP_DOCKERFILE="$here/Dockerfile.app"

if (( build )); then
  docker build -q -f "$here/Dockerfile.app" --build-context entrypoint="$here" -t "$APP_IMAGE" "$ctx" >/dev/null
  docker build -q -f "$here/Dockerfile.tools" -t "$TOOLS_IMAGE" "$repo" >/dev/null
fi

"${compose[@]}" up -d --no-build --remove-orphans db
"${compose[@]}" up -d --no-build --wait db
if [[ $datagen == 1 || ! -f $WORK_DIR/manifest.json ]]; then
  echo "datagen: $scale ($DATAGEN_FLAGS)"
  "${compose[@]}" run --rm --no-deps datagen
fi
"${compose[@]}" up -d --no-build --remove-orphans

target="http://127.0.0.1:$ROUTER_PORT"
for _ in $(seq 1 90); do
  ok=1
  for cc in JP GB US; do
    code=$(curl -s -o /dev/null -w '%{http_code}' -H "X-Country-Code: $cc" --max-time 2 "$target/api/me" || true)
    [[ $code == 401 ]] || ok=0
  done
  (( ok )) && break
  sleep 1
done
if (( ! ok )); then
  echo "app did not become ready: $target" >&2
  "${compose[@]}" logs --tail 20 asia europe america router >&2
  exit 1
fi

status=0
if (( in_docker )) || ! command -v go >/dev/null; then
  export BENCH_FLAGS="${bench_flags[*]}"
  "${compose[@]}" run --rm --no-deps bench | tee "$WORK_DIR/bench.log" || status=$?
else
  (cd "$repo/bench" && go build -o "$WORK_DIR/isureal-bench" ./cmd/isureal-bench)
  "$WORK_DIR/isureal-bench" -target "$target" -manifest "$WORK_DIR/manifest.json" -out "$WORK_DIR/result.json" "${bench_flags[@]}" | tee "$WORK_DIR/bench.log" || status=$?
fi
echo "result: $WORK_DIR/result.json"

if (( ! keep )); then
  "${compose[@]}" stop >/dev/null
fi
exit $status
