#!/bin/bash
# #17 で 1 文にしたハンドラ (GET/PATCH/DELETE /api/posts/:postId、DELETE /api/comments/:commentId) を、
# 手元の Docker の PostgreSQL 16 に 001/002/007-image-location/004/005 を流した空の DB で動かす
# (go/roundtrip_pg_test.go)。005-roundtrips.sql を 2 回流しても失敗しないこと (initialize が毎回流す) も見る。
# 接続時にまとめて Prepare する仕組み (go/prepared_warm.go) も、同じ DB で 1 往復になること・応答が変わらないことを見る。
# 使い方: app/webapp/sql/tests/roundtrips.sh
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
NAME="isureal-roundtrips-test-$$"
PORT=${PORT:-55432}
PSQL=(docker exec -i -e PGOPTIONS=-cclient_min_messages=warning "$NAME" psql -U postgres -d postgres -v ON_ERROR_STOP=1 -qAtX)

docker run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=pw -p "127.0.0.1:$PORT:5432" postgres:16 >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
until docker exec "$NAME" pg_isready -U postgres -q 2>/dev/null; do sleep 0.5; done
sleep 1

for file in 001-schema.sql 002-indexes.sql 007-image-location.sql 004-functions.sql 005-roundtrips.sql 005-roundtrips.sql; do
	"${PSQL[@]}" < "$file" >/dev/null
done

cd ../go
ISUREAL_TEST_DSN="postgres://postgres:pw@127.0.0.1:$PORT/postgres?sslmode=disable" \
	go test -count=1 -run 'TestRoundTripHandlersOnPostgres|TestWarmPreparedStatementsOnPostgres|TestWarmedHandlersRespondTheSame' -v . 2>&1 |
	grep -E '^(=== RUN|--- |ok|FAIL|PASS)|roundtrip|prepared_warm'
