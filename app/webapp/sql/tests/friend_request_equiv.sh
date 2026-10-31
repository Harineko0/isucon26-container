#!/bin/bash
# 友達申請の送信・承認・拒否・取り下げの関数を、${BASE_REF:-origin/main} の版 (*_base に改名して入れる) と
# このブランチの版で比べる (tests/friend_request_equiv.sql)。手元の Docker の PostgreSQL 16 だけを使う。
# 使い方: app/webapp/sql/tests/friend_request_equiv.sh (SEEDS="0.42 0.1" で乱数の種を変える)
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
BASE_REF=${BASE_REF:-origin/main}
NAME="isureal-friend-request-equiv-$$"
PSQL=(docker exec -i -e PGOPTIONS=-cclient_min_messages=notice "$NAME" psql -U postgres -v ON_ERROR_STOP=1 -qAtX)
docker run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=pw postgres:16 >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
until docker exec "$NAME" pg_isready -U postgres -q 2>/dev/null; do sleep 0.5; done
sleep 1
for file in 001-schema.sql 002-indexes.sql 007-image-location.sql 004-functions.sql 005-roundtrips.sql; do
	"${PSQL[@]}" < "$file" >/dev/null 2>&1
done
rename_base() {
	sed -e 's/isureal_lock_pair(/isureal_lock_pair_base(/g' \
		-e 's/isureal_relationship(/isureal_relationship_base(/g' \
		-e 's/isureal_transition_friend_request(/isureal_transition_friend_request_base(/g' \
		-e 's/isureal_transition_friend_request_authorized(/isureal_transition_friend_request_authorized_base(/g'
}
{
	git show "$BASE_REF:app/webapp/sql/004-functions.sql" | sed -n \
		-e '/^CREATE OR REPLACE FUNCTION isureal_lock_pair(/,/^END \$\$;/p' \
		-e '/^CREATE OR REPLACE FUNCTION isureal_relationship(/,/^END \$\$;/p' \
		-e '/^CREATE OR REPLACE FUNCTION isureal_transition_friend_request(/,/^END \$\$;/p'
	git show "$BASE_REF:app/webapp/sql/005-roundtrips.sql" | sed -n \
		-e '/^CREATE OR REPLACE FUNCTION isureal_transition_friend_request_authorized(/,/^END \$\$;/p'
} | rename_base | "${PSQL[@]}"
for seed in ${SEEDS:-0.42 0.1 -0.7}; do
	sed "s/setseed(0.42)/setseed($seed)/" tests/friend_request_equiv.sql | "${PSQL[@]}"
done
