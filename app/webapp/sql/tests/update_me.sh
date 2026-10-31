#!/bin/bash
# #83 の isureal_update_me (PATCH /api/me) を、手元の Docker の PostgreSQL 16 で確かめる。
#   - 退会済みの利用者の行を書き換えず not_found を返すこと (変更前の ref では書き換えて ok を返していた)
#   - active な利用者は従来どおり更新されること、存在しない利用者は not_found
#   - 退会 (isureal_delete_account) のコミット前に update_me が来ても、退会のコミットを待ってから
#     not_found になり、行を書き換えないこと
#   - 004 / 005 を何度流しても失敗しないこと (initialize が毎回流す)
# 使い方: app/webapp/sql/tests/update_me.sh [比較対象の git ref (既定 origin/main)]
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
BASE_REF="${1:-origin/main}"
NAME="isureal-update-me-test-$$"
PSQL=(docker exec -i -e PGOPTIONS=-cclient_min_messages=warning "$NAME" psql -U postgres -d postgres -v ON_ERROR_STOP=1 -qAtX)

docker run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=pw postgres:16 >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
until docker exec "$NAME" pg_isready -U postgres -q 2>/dev/null; do sleep 0.5; done
sleep 1

load() { # $1 = 004 の中身, $2 = 005 の中身
	"${PSQL[@]}" <<<"$1" >/dev/null
	"${PSQL[@]}" <<<"$2" >/dev/null
}
"${PSQL[@]}" < 001-schema.sql >/dev/null

update_me_deleted() {
	"${PSQL[@]}" <<SQL
TRUNCATE users CASCADE;
INSERT INTO users(id, username, secret_key_hash, display_name, region, status) VALUES ('d', 'd', 'x', 'D', 'asia', 'deleted');
SELECT '$1 deleted: ' || o_result FROM isureal_update_me('d', 'D2', 'bio', 'europe');
SELECT '$1 deleted: row unchanged=' || (display_name = 'D' AND bio = '' AND region = 'asia') FROM users WHERE id = 'd';
SQL
}
load "$(git show "$BASE_REF:app/webapp/sql/004-functions.sql")" "$(git show "$BASE_REF:app/webapp/sql/005-roundtrips.sql")"
update_me_deleted "before ($BASE_REF)"
load "$(cat 004-functions.sql)" "$(cat 005-roundtrips.sql)"
load "$(cat 004-functions.sql)" "$(cat 005-roundtrips.sql)"
update_me_deleted "after"

"${PSQL[@]}" <<'SQL'
TRUNCATE users CASCADE;
INSERT INTO users(id, username, secret_key_hash, display_name, region)
VALUES ('a', 'a', 'x', 'A', 'asia'), ('c', 'c', 'x', 'C', 'asia');
SELECT 'active: ' || o_result || ' ' || o_display_name || ' ' || o_bio || ' ' || o_region || ' ' || o_status
FROM isureal_update_me('a', 'A2', NULL, 'europe');
SELECT 'missing: ' || o_result FROM isureal_update_me('zzz', 'Z', NULL, NULL);
SQL
# 退会と同時: 退会のトランザクションがコミットする前に update_me が来る。
(
	"${PSQL[@]}" <<'SQL'
BEGIN;
SELECT isureal_delete_account('c') \gset
SELECT pg_sleep(1);
COMMIT;
SQL
) >/dev/null &
sleep 0.3
"${PSQL[@]}" <<'SQL'
SELECT 'concurrent delete: ' || o_result FROM isureal_update_me('c', 'C2', NULL, NULL);
SQL
wait
"${PSQL[@]}" <<'SQL'
SELECT 'concurrent delete: row unchanged=' || (display_name = 'C' AND status = 'deleted') FROM users WHERE id = 'c';
SQL
