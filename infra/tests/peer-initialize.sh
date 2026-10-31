#!/usr/bin/env bash
# Exercise the deployed nginx site, Go app and init.sh in three isolated regions.
set -euo pipefail
cd "$(dirname "$0")/../.."
repo=$PWD
prefix="isureal-peer-test-$$"
scratch=$(mktemp -d)
containers=()
cleanup() {
  if (( ${#containers[@]} )); then docker rm -fv "${containers[@]}" >/dev/null 2>&1 || true; fi
  docker network rm "$prefix" >/dev/null 2>&1 || true
  rm -rf "$scratch"
}
trap cleanup EXIT
docker network create --internal --subnet 192.168.0.0/20 "$prefix" >/dev/null
start() {
  local name="$prefix-$1"
  shift
  docker run -d --name "$name" "$@" >/dev/null
  containers+=("$name")
}
start db --network "$prefix" --ip 192.168.4.10 -e POSTGRES_PASSWORD=test postgres:16 -c max_connections=300
sql=(docker exec -i -e PGOPTIONS=-cclient_min_messages=warning "$prefix-db" psql -U postgres -v ON_ERROR_STOP=1 -qAtX)
for _ in {1..60}; do
  docker exec "$prefix-db" pg_isready -h 127.0.0.1 -q && break
  sleep 0.5
done
"${sql[@]}" <app/webapp/sql/001-schema.sql >/dev/null
"${sql[@]}" -c "INSERT INTO users (id,username,secret_key_hash,display_name,region) VALUES ('u','seed',repeat('1',64),'Seed','asia')"
"${sql[@]}" <app/webapp/sql/003-initial-snapshot.sql >/dev/null

arch=$(docker image inspect postgres:16 --format '{{.Architecture}}')
(cd app/webapp/go && CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -o "$scratch/isureal" ./cmd/isureal)
listen=$(sed -n 's/^Environment=\(ISUREAL_LISTEN_HOST=.*\)/\1/p' infra/app/etc/systemd/system/isureal-go.service)
regions=(asia europe america)
for i in 0 1 2; do
  region=${regions[$i]}
  start "$region" --network "$prefix" --ip "192.168.$((i+1)).10" \
    -v "$scratch/isureal:/bin/isureal:ro" -v "$repo/app/webapp/sql:/webapp/sql:ro" -w /webapp/go \
    -e "$listen" -e ISUREAL_REGION="$region" -e ISUREAL_DB_HOST=192.168.4.10 \
    -e ISUREAL_DB_USER=postgres -e ISUREAL_DB_NAME=postgres -e ISUREAL_DB_PASSWORD=test \
    --entrypoint /bin/isureal postgres:16
  start "$region-nginx" --network "container:$prefix-$region" \
    -v "$repo/infra/app/etc/nginx/sites-available/isureal.conf:/site.conf:ro" nginx:1.26-alpine sh -ec '
    sed -i "1i thread_pool image_reads threads=4;" /etc/nginx/nginx.conf
    cp /site.conf /etc/nginx/conf.d/default.conf
    nginx -t && exec nginx -g "daemon off;"'
done
# Same socket source IP as public ingress. No host ports or EC2 connections.
start router --network "$prefix" --ip 192.168.10.10 --entrypoint sleep nginx:1.26-alpine 3600
http() {
  local source=$1
  shift
  docker exec "$prefix-$source" curl -sS --max-time 20 -w '%{http_code}' -o /dev/null "$@"
}
expect() {
  local want=$1 got
  shift
  got=$(http "$@")
  [[ $got == "$want" ]] || { echo "expected $want, got $got (source=$1)" >&2; exit 1; }
}
header="X-Isureal-Internal: $(printf '%s' isureal-internal-image-store-v1 | openssl dgst -sha256 -hmac test -r | cut -d' ' -f1)"
id=00000000-0000-4000-8000-000000000001
for n in 1 2 3; do
  host="http://192.168.$n.10"
  # Cross-host request even when the destination is asia.
  peer=asia-nginx; if [[ $n == 1 ]]; then peer=europe-nginx; fi
  for _ in {1..50}; do [[ $(http router "$host/api/me" 2>/dev/null || true) == 401 ]] && break; sleep 0.2; done
  expect 204 "$peer" -H "$header" -X PUT --data-binary 'fixture image' "$host/api/internal/images/$id"
  expect 200 "$peer" -H "$header" "$host/api/internal/images/$id"
  spoof=(-H "$header" -H 'X-Client-IP: 192.168.2.10' -H 'X-Forwarded-For: 192.168.2.10')
  expect 404 router "${spoof[@]}" "$host/api/internal/images/$id"
  expect 404 router "${spoof[@]}" -X PUT --data-binary corrupt "$host/api/internal/images/$id"
  expect 404 router "${spoof[@]}" -X POST "$host/api/internal/initialize/reset"
  expect 403 router "$host/api/images/$id"
  if http router "$host:8080/api/internal/images/$id" >/dev/null 2>&1; then echo 'Go port reachable' >&2; exit 1; fi
done
echo 'PASS public GET/PUT/reset blocked, peer PUT/GET allowed in all regions'

init_url=http://192.168.1.10/api/initialize
# Make the remote reset fail after preparation succeeds. Repair and retry via the real API.
docker exec "$prefix-america-nginx" sh -ec 'sed -i "/listen 80 default_server;/a\  location = /api/internal/initialize/reset { return 503; }" /etc/nginx/conf.d/default.conf; nginx -s reload'
for _ in {1..50}; do
  [[ $(http asia-nginx -H "$header" http://192.168.3.10/api/internal/initialize/reset) == 503 ]] && break
  sleep 0.1
done
expect 500 router -X POST "$init_url"
expect 200 asia-nginx -H "$header" "http://192.168.3.10/api/internal/images/$id"
docker exec "$prefix-america-nginx" sh -ec 'cp /site.conf /etc/nginx/conf.d/default.conf; nginx -s reload'
# Create an actual queued BAN and leave a different IP just below the threshold.
banned_login=(--json '{}' -H 'X-Client-IP: 203.0.113.8' http://192.168.2.10/api/auth/login)
for _ in {1..10}; do expect 401 router "${banned_login[@]}"; done
expect 403 router "${banned_login[@]}"
login=(--json '{}' -H 'X-Client-IP: 203.0.113.7' http://192.168.2.10/api/auth/login)
for _ in {1..9}; do expect 401 router "${login[@]}"; done
expect 200 router -X POST "$init_url"
# Two more login failures must not complete the previous region's 10-failure window.
expect 401 router "${login[@]}"
expect 401 router "${login[@]}"
for n in 1 2 3; do
  peer=asia-nginx; if [[ $n == 1 ]]; then peer=europe-nginx; fi
  expect 404 "$peer" -H "$header" "http://192.168.$n.10/api/internal/images/$id"
  expect 401 router -H 'X-Client-IP: 203.0.113.8' "http://192.168.$n.10/api/me"
done
echo 'PASS peer reset failure reached /api/initialize, retry cleared remote images and login failure history'
