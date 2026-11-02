#!/bin/bash
# #153: PgBouncer (本番と同じ 1.26.1) を通したアプリの接続で plan_cache_mode = force_generic_plan になることを、
# 手元の Docker の PostgreSQL 16 と PgBouncer で確かめる (go/pgbouncer_pg_test.go)。
# DB とプーラーはネットワーク名前空間を共有し、本番と同じ loopback と SCRAM 認証で接続する。
# 使い方: app/webapp/sql/tests/pgbouncer_plan_cache.sh
set -euo pipefail
cd -- "$(dirname -- "$0")/../.."
INI=../../infra/hosts/asia2/etc/isureal/pgbouncer.ini
PG="isureal-bouncer-test-$$"
PGB="$PG-pool"
PORT=${PORT:-56432}
WORK=$(mktemp -d)
cleanup() { docker rm -f "$PGB" "$PG" >/dev/null 2>&1 || true; rm -r "$WORK"; }
trap cleanup EXIT

docker run -d --rm --name "$PG" -p "127.0.0.1:$PORT:6432" -e POSTGRES_USER=isureal \
	-e POSTGRES_PASSWORD=isureal -e POSTGRES_DB=isureal postgres:16 >/dev/null
until docker exec "$PG" pg_isready -h 127.0.0.1 -U isureal -q 2>/dev/null; do sleep 0.5; done

sed -e 's/^listen_addr = .*/listen_addr = 0.0.0.0/' \
    -e 's|^auth_file = .*|auth_file = /etc/pgbouncer/userlist.txt|' "$INI" > "$WORK/pgbouncer.ini"
docker exec -i "$PG" psql -XAtqw -U isureal -v ON_ERROR_STOP=1 > "$WORK/userlist.txt" <<'SQL'
SELECT '"' || rolname || '" "' || rolpassword || '"'
FROM pg_authid WHERE rolname = current_user AND rolpassword LIKE 'SCRAM-SHA-256$%';
SQL
test -s "$WORK/userlist.txt"
MSYS_NO_PATHCONV=1 docker run -d --rm --name "$PGB" --network "container:$PG" \
	-v "$(cygpath -w "$WORK" 2>/dev/null || echo "$WORK"):/etc/pgbouncer:ro" \
	--entrypoint pgbouncer edoburu/pgbouncer:v1.26.1-p0 /etc/pgbouncer/pgbouncer.ini >/dev/null
for attempt in {1..30}; do
	if docker exec -e PGPASSWORD=isureal "$PG" psql -h 127.0.0.1 -p 6432 -U isureal -XAtqc 'SELECT 1' >/dev/null 2>&1; then break; fi
	if [ "$attempt" = 30 ]; then docker logs "$PGB"; exit 1; fi
	sleep 0.2
done

cd go
ISUREAL_TEST_BOUNCER_HOST=127.0.0.1 ISUREAL_TEST_BOUNCER_PORT=$PORT \
	go test -count=1 -run 'TestPgBouncerPlanCacheModeOnPostgres' -v . 2>&1 | grep -E '^(=== RUN|--- |ok|FAIL|PASS)|plan_cache_mode|PgBouncer'
