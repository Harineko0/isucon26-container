#!/bin/bash
# Local PostgreSQL 16 regression check for #150. No production connections.
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
NAME="isureal-function-reads-$$"
PSQL=(docker exec -i -e PGOPTIONS=-cclient_min_messages=warning "$NAME" psql -U postgres -v ON_ERROR_STOP=1 -qAtX)
docker run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=pw postgres:16 >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
until docker exec "$NAME" pg_isready -U postgres -q 2>/dev/null; do sleep 0.5; done
for file in 001-schema.sql 002-indexes.sql 007-image-location.sql 004-functions.sql 005-roundtrips.sql 004-functions.sql; do
  "${PSQL[@]}" < "$file" >/dev/null
done
# Pin the comparison to the implementation before #150, not the moving main.
git show 5375021360b73482ad0c153436b40acea7f03ff1:app/webapp/sql/004-functions.sql |
  sed -n '/^CREATE OR REPLACE FUNCTION isureal_post_access(/,/^END \$\$;/p' |
  sed 's/FUNCTION isureal_post_access(/FUNCTION isureal_post_access_baseline(/' |
  "${PSQL[@]}"
"${PSQL[@]}" < tests/roundtrips.sql
"${PSQL[@]}" < tests/function_reads.sql
