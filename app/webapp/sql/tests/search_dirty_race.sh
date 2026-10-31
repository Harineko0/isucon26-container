#!/bin/bash
# #148: 検索用の正規化の保存 (saveSearchNorms) / dirty の掃除 (cleanSearchDirty) と、表示名の変更 (トリガー) が
# 並行しても、「正規化が今の表示名と違うのに dirty の行が無い」利用者 (新しい名前で検索に出ない) を作らないことを、
# 使い捨ての Docker の PostgreSQL 16 で 2〜3 本の接続を使って確かめる。
#   1. 保存の文がスナップショットを取った後に改名が確定する (保存を user_search_norms の行ロックで止めて再現)
#   2. 別の台の古い表示名の保存が、新しい表示名の保存の後に届く
#   3. 掃除の文がスナップショットを取った後に改名が確定する (掃除を dirty の行ロックで止めて再現)
#   4. 1 文で保存する 2 人のうち、2 人目の改名が保存の途中で確定する (READ COMMITTED の再確認を通る)
# 使い方: app/webapp/sql/tests/search_dirty_race.sh        (このブランチの SQL)
#         BASE=1 app/webapp/sql/tests/search_dirty_race.sh (修正前の SQL。失敗するのが正しい)
# SQL は go/user_read_api.go の saveSearchNorms / cleanSearchDirty の写し (変えるときは両方)。
set -euo pipefail
cd -- "$(dirname -- "$0")/.."

NAME="isureal-dirty-race-$$"
docker run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=pw postgres:16 >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
until docker exec "$NAME" pg_isready -U postgres -q 2>/dev/null; do sleep 0.5; done
sleep 1
PSQL=(docker exec -i -e PGOPTIONS='-c client_min_messages=warning' "$NAME" psql -U postgres -d postgres -v ON_ERROR_STOP=1 -qAtX)

"${PSQL[@]}" < 001-schema.sql >/dev/null
if [ "${BASE:-}" = 1 ]; then
	echo "mode: BASE (origin/main の SQL)"
	git show origin/main:app/webapp/sql/002-indexes.sql | "${PSQL[@]}" >/dev/null
	save_sql() { # $1: 'id:name,id:name'
		cat <<SQL
WITH b AS (SELECT split_part(x, ':', 1) AS id, split_part(x, ':', 2) AS name FROM unnest(string_to_array('$1', ',')) x),
saved AS (
  INSERT INTO user_search_norms (user_id, username_norm, display_name, display_name_norm)
  SELECT id, id, name, lower(name) FROM b ORDER BY id
  ON CONFLICT (user_id) DO UPDATE SET username_norm = EXCLUDED.username_norm,
    display_name = EXCLUDED.display_name, display_name_norm = EXCLUDED.display_name_norm
  RETURNING user_id, display_name
)
DELETE FROM user_search_dirty d USING saved, users u
WHERE d.user_id = saved.user_id AND u.id = saved.user_id AND u.display_name = saved.display_name;
SQL
	}
	CLEAN_SQL="DELETE FROM user_search_dirty d
  WHERE NOT EXISTS (
    SELECT 1 FROM users u LEFT JOIN user_search_norms s ON s.user_id = u.id
    WHERE u.id = d.user_id AND s.display_name IS DISTINCT FROM u.display_name);"
else
	echo "mode: this branch"
	"${PSQL[@]}" < 002-indexes.sql >/dev/null
	save_sql() {
		cat <<SQL
WITH batch AS (
  SELECT split_part(x, ':', 1) AS user_id, split_part(x, ':', 1) AS username_norm,
         split_part(x, ':', 2) AS display_name, lower(split_part(x, ':', 2)) AS display_name_norm
  FROM unnest(string_to_array('$1', ',')) x
), locked AS (
  SELECT b.* FROM batch b JOIN user_search_dirty d ON d.user_id = b.user_id AND d.display_name = b.display_name
  FOR UPDATE OF d SKIP LOCKED
), saved AS (
  INSERT INTO user_search_norms (user_id, username_norm, display_name, display_name_norm)
  SELECT user_id, username_norm, display_name, display_name_norm FROM locked
  ON CONFLICT (user_id) DO UPDATE SET username_norm = EXCLUDED.username_norm,
    display_name = EXCLUDED.display_name, display_name_norm = EXCLUDED.display_name_norm
  RETURNING user_id
)
DELETE FROM user_search_dirty d USING saved WHERE d.user_id = saved.user_id;
SQL
	}
	CLEAN_SQL="DELETE FROM user_search_dirty WHERE user_id IN (
    SELECT d.user_id FROM user_search_dirty d
    WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = d.user_id)
       OR EXISTS (SELECT 1 FROM user_search_norms s JOIN users u ON u.id = s.user_id
                  WHERE s.user_id = d.user_id AND s.display_name = d.display_name AND u.display_name = d.display_name)
    FOR UPDATE OF d SKIP LOCKED);"
fi

sql() { "${PSQL[@]}" -c "$1"; }
add_user() { sql "INSERT INTO users(id, username, secret_key_hash, display_name, region) VALUES ('$1', '$1', 'x', '$2', 'asia')"; }
rename() { echo "UPDATE users SET display_name = '$2' WHERE id = '$1';"; }
# ロック待ちの接続が n 本になるまで待つ。
wait_locks() {
	for _ in $(seq 1 100); do
		[ "$(sql "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'")" -ge "$1" ] && return
		sleep 0.1
	done
	echo "  (timeout waiting for $1 lock waiter(s))"
}
# ロックを持って止まっている接続 (idle in transaction) ができるまで待つ。
wait_holder() {
	for _ in $(seq 1 100); do
		[ "$(sql "SELECT count(*) FROM pg_stat_activity WHERE state = 'idle in transaction'")" -ge 1 ] && return
		sleep 0.1
	done
	echo "  (timeout waiting for the lock holder)"
}
# 文がロック待ちになるか終わるまで待つ (修正後の掃除は SKIP LOCKED なので待たずに終わる)。
wait_blocked_or_done() {
	for _ in $(seq 1 100); do
		kill -0 "$1" 2>/dev/null || return 0
		[ "$(sql "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock'")" -ge 1 ] && return
		sleep 0.1
	done
}
# 正規化が今の表示名と違い、dirty の行も無い利用者 = 新しい名前で検索に出ない利用者。
lost() { sql "SELECT coalesce(string_agg(u.id || '(' || u.display_name || ' norm=' || coalesce(s.display_name, '-') || ')', ' '), '')
  FROM users u LEFT JOIN user_search_norms s ON s.user_id = u.id
  WHERE s.display_name IS DISTINCT FROM u.display_name
    AND NOT EXISTS (SELECT 1 FROM user_search_dirty d WHERE d.user_id = u.id)"; }

fail=0
check() {
	local l
	l=$(lost)
	if [ -n "$l" ]; then echo "  FAIL $1: not findable by the new name: $l"; fail=1; else echo "  ok   $1"; fi
}

# 1. 保存のスナップショットの後に改名が確定する。
add_user s1 Alpha
sql "INSERT INTO user_search_norms VALUES ('s1', 's1', 'Old', 'old')"
{ echo "BEGIN; SELECT 1 FROM user_search_norms WHERE user_id = 's1' FOR UPDATE;"; sleep 3; echo "COMMIT;"; } | "${PSQL[@]}" >/dev/null &
L=$!
wait_holder
save_sql s1:Alpha | "${PSQL[@]}" >/dev/null &
S=$!
wait_locks 1
rename s1 Beta | "${PSQL[@]}" >/dev/null &
R=$!
wait $L $S $R
check "1 save vs rename"

# 2. 古い表示名の保存が新しい保存の後に届く (別の台の保存待ち)。
add_user s2 Alpha
rename s2 Beta | "${PSQL[@]}" >/dev/null
save_sql s2:Beta | "${PSQL[@]}" >/dev/null
save_sql s2:Alpha | "${PSQL[@]}" >/dev/null
check "2 stale save after fresh save"

# 3. 掃除のスナップショットの後に改名が確定する。
add_user s3 Alpha
sql "INSERT INTO user_search_norms VALUES ('s3', 's3', 'Alpha', 'alpha')"
{ echo "BEGIN; SELECT 1 FROM user_search_dirty WHERE user_id = 's3' FOR UPDATE;"; sleep 3; echo "COMMIT;"; } | "${PSQL[@]}" >/dev/null &
L=$!
wait_holder
echo "$CLEAN_SQL" | "${PSQL[@]}" >/dev/null &
C=$!
wait_blocked_or_done $C
rename s3 Beta | "${PSQL[@]}" >/dev/null &
R=$!
wait $L $C $R
echo "$CLEAN_SQL" | "${PSQL[@]}" >/dev/null
check "3 clean vs rename"

# 4. 2 人を 1 文で保存する途中で、2 人目の改名が確定する。
add_user s4a Alpha
add_user s4b Alpha
sql "INSERT INTO user_search_norms VALUES ('s4a', 's4a', 'Old', 'old')"
{ echo "BEGIN; SELECT 1 FROM user_search_norms WHERE user_id = 's4a' FOR UPDATE;"; sleep 3; echo "COMMIT;"; } | "${PSQL[@]}" >/dev/null &
L=$!
wait_holder
save_sql s4a:Alpha,s4b:Alpha | "${PSQL[@]}" >/dev/null &
S=$!
wait_locks 1
rename s4b Beta | "${PSQL[@]}" >/dev/null &
R=$!
wait $L $S $R
check "4 batch save, rename of the 2nd user mid-statement"

# 後片付けの経路: 今の名前で保存し直し、掃除すると dirty が空になる (保存も掃除も止まらない)。
save_sql "s1:Beta,s2:Beta,s3:Beta,s4a:Alpha,s4b:Beta" | "${PSQL[@]}" >/dev/null
echo "$CLEAN_SQL" | "${PSQL[@]}" >/dev/null
check "5 resave with current names"
echo "  dirty rows left: $(sql "SELECT count(*) FROM user_search_dirty")"

if [ "${BASE:-}" != 1 ]; then
	# 002 を何度流しても失敗しない (initialize が毎回流す) こと、列の無い古い表からの移行で NULL が埋まること。
	"${PSQL[@]}" < 002-indexes.sql >/dev/null
	sql "ALTER TABLE user_search_dirty DROP COLUMN display_name" >/dev/null
	sql "INSERT INTO user_search_dirty (user_id) VALUES ('s1')" >/dev/null
	"${PSQL[@]}" < 002-indexes.sql >/dev/null
	"${PSQL[@]}" < 002-indexes.sql >/dev/null
	m=$(sql "SELECT count(*) FILTER (WHERE d.display_name IS DISTINCT FROM u.display_name) FROM user_search_dirty d JOIN users u ON u.id = d.user_id")
	if [ "$m" = 0 ]; then echo "  ok   6 migration from the old table backfills display_name"; else echo "  FAIL 6 migration: $m rows"; fail=1; fi
fi
exit $fail
