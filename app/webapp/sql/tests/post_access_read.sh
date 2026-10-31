#!/bin/bash
# 読み取り用の isureal_post_access_read が isureal_post_access と同じ can_view / can_comment を返すことの確認。
# 使い捨ての Docker の PostgreSQL 16 で行う (本番には接続しない)。
# 使い方: app/webapp/sql/tests/post_access_read.sh
set -euo pipefail
cd -- "$(dirname -- "$0")/.."

NAME="isureal-post-access-read-$$"
docker run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=pw postgres:16 >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
until docker exec "$NAME" pg_isready -U postgres -q 2>/dev/null; do sleep 0.5; done
sleep 1
PSQL=(docker exec -i -e PGOPTIONS='-c client_min_messages=warning' "$NAME" psql -U postgres -d postgres -v ON_ERROR_STOP=1 -qAtX)

for file in 001-schema.sql 002-indexes.sql 007-image-location.sql 004-functions.sql 005-roundtrips.sql 006-image-storage.sql 007-feed-indexes.sql 008-image-partitions.sql; do
	"${PSQL[@]}" < "$file" >/dev/null
done
"${PSQL[@]}" < tests/post_access_read.sql 2>&1
