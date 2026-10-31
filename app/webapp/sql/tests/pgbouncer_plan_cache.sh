#!/bin/bash
# #153: PgBouncer (本番と同じ 1.26.1) を通したアプリの接続で plan_cache_mode = force_generic_plan になることを、
# 手元の Docker の PostgreSQL 16 と PgBouncer で確かめる (go/pgbouncer_pg_test.go)。
# [databases] の行は infra/hosts/asia1/etc/isureal/pgbouncer.ini から取り、host だけ差し替える。
# 使い方: app/webapp/sql/tests/pgbouncer_plan_cache.sh
set -euo pipefail
cd -- "$(dirname -- "$0")/../.."
INI=../../infra/hosts/asia1/etc/isureal/pgbouncer.ini
NET="isureal-bouncer-test-$$"
PG="$NET-pg"
PGB="$NET-pgb"
PORT=${PORT:-56432}
WORK=$(mktemp -d)
cleanup() { docker rm -f "$PGB" "$PG" >/dev/null 2>&1 || true; docker network rm "$NET" >/dev/null 2>&1 || true; rm -rf "$WORK"; }
trap cleanup EXIT

docker network create "$NET" >/dev/null
docker run -d --rm --name "$PG" --network "$NET" -e POSTGRES_USER=isureal -e POSTGRES_PASSWORD=isureal \
	-e POSTGRES_DB=isureal -e POSTGRES_HOST_AUTH_METHOD=trust postgres:16 >/dev/null
until docker exec "$PG" pg_isready -U isureal -q 2>/dev/null; do sleep 0.5; done
sleep 1

dbline=$(grep -E '^isureal *=' "$INI" | sed -E "s/host=[^ ]+/host=$PG/")
cat > "$WORK/pgbouncer.ini" <<INI
[databases]
$dbline
[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 6432
auth_type = plain
auth_file = /etc/pgbouncer/userlist.txt
$(grep -E '^(pool_mode|default_pool_size|max_prepared_statements) *=' "$INI")
INI
echo '"isureal" "isureal"' > "$WORK/userlist.txt"
MSYS_NO_PATHCONV=1 docker run -d --rm --name "$PGB" --network "$NET" -p "127.0.0.1:$PORT:6432" \
	-v "$(cygpath -w "$WORK" 2>/dev/null || echo "$WORK"):/etc/pgbouncer:ro" \
	--entrypoint pgbouncer edoburu/pgbouncer:latest /etc/pgbouncer/pgbouncer.ini >/dev/null
sleep 2

cd go
ISUREAL_TEST_BOUNCER_HOST=127.0.0.1 ISUREAL_TEST_BOUNCER_PORT=$PORT \
	go test -count=1 -run 'TestPgBouncerPlanCacheModeOnPostgres' -v . 2>&1 | grep -E '^(=== RUN|--- |ok|FAIL|PASS)|plan_cache_mode|PgBouncer'
