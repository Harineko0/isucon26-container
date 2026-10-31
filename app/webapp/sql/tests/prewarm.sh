#!/bin/bash
# initialize 後 / 起動時の pg_prewarm (go/prewarm.go) を、手元の Docker の PostgreSQL 16 に init.sh と同じ順で
# 001〜008 を流した空の DB で動かす (go/prewarm_test.go)。
#   - 候補の表・索引の名前が実在すること (綴り違いは黙って飛ぶので、ここで見つける)
#   - CREATE EXTENSION IF NOT EXISTS pg_prewarm と pg_prewarm() が superuser で通ること
#   - 2 回目は debounce で飛ぶこと
# 使い方: app/webapp/sql/tests/prewarm.sh
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
NAME="isureal-prewarm-test-$$"
PORT=${PORT:-55433}
PSQL=(docker exec -i -e PGOPTIONS=-cclient_min_messages=warning "$NAME" psql -U postgres -d postgres -v ON_ERROR_STOP=1 -qAtX)

docker run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=pw -p "127.0.0.1:$PORT:5432" postgres:16 \
	-c shared_buffers=128MB >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
until docker exec "$NAME" pg_isready -U postgres -q 2>/dev/null; do sleep 0.5; done
sleep 1

"${PSQL[@]}" < 001-schema.sql >/dev/null
for file in 002-indexes.sql 007-image-location.sql 004-functions.sql 005-roundtrips.sql 006-image-storage.sql 007-feed-indexes.sql 008-image-partitions.sql; do
	"${PSQL[@]}" < "$file" >/dev/null
done

cd ../go
ISUREAL_TEST_DSN="postgres://postgres:pw@127.0.0.1:$PORT/postgres?sslmode=disable" \
	go test -count=1 -run 'TestPrewarm' -v . 2>&1 | grep -E '^(=== RUN|--- |ok|FAIL|PASS)|prewarm'
