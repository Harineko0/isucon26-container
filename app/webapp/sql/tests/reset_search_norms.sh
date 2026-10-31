#!/bin/bash
# reset.sql が、消した利用者の user_search_norms の行を消して表を縮めることを、使い捨ての PostgreSQL で確かめる。
# 初期データ (境界より前の利用者) → initialize → ベンチ相当 (境界より後の利用者の登録とその正規化の行、
# 以前の initialize で残った行、初期データの利用者の改名) → initialize を流し、
#   - 境界より後の利用者が消え、users に無い利用者の行が user_search_norms に残らないこと
#   - 初期データの利用者の行は (正規化が古くても) 残ること
#   - user_search_norms のページ数が減ること (VACUUM が空になった末尾を切り詰める)
#   - init.sh と同じ順でファイルを何度流しても失敗しないこと (initialize が毎回流す)
# を確かめる。
# 使い方: app/webapp/sql/tests/reset_search_norms.sh
#   既定は Docker の postgres:16。Docker が無ければ PG_LOCAL=1 を付けると、PATH の initdb / pg_ctl で
#   一時ディレクトリにクラスタを作り、127.0.0.1 だけで待ち受ける。どちらもサーバーの DB には繋がない。
set -euo pipefail
cd -- "$(dirname -- "$0")/.."

if [ "${PG_LOCAL:-}" = 1 ]; then
	DIR=$(mktemp -d)
	PORT=$((40000 + $$ % 20000))
	trap 'pg_ctl -D "$DIR/data" -m immediate stop >/dev/null 2>&1 || true; rm -rf "$DIR"' EXIT
	initdb -D "$DIR/data" -U postgres -A trust >/dev/null
	pg_ctl -D "$DIR/data" -l "$DIR/log" -w \
		-o "-c listen_addresses=127.0.0.1 -p $PORT -c unix_socket_directories=''" start >/dev/null
	PSQL=(env PGOPTIONS='-c client_min_messages=warning' psql -h 127.0.0.1 -p "$PORT" -U postgres -d postgres -v ON_ERROR_STOP=1 -qAtX)
else
	NAME="isureal-reset-test-$$"
	docker run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=pw postgres:16 >/dev/null
	trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
	until docker exec "$NAME" pg_isready -U postgres -q 2>/dev/null; do sleep 0.5; done
	sleep 1
	PSQL=(docker exec -i -e PGOPTIONS='-c client_min_messages=warning' "$NAME" psql -U postgres -d postgres -v ON_ERROR_STOP=1 -qAtX)
fi
echo "server: $("${PSQL[@]}" -c 'SHOW server_version')"

# init.sh と同じファイルを同じ順で流す (一覧は init.sh から取り出すので、ずれない)。
FILES=$(sed -n 's/^for file in \(.*\); do$/\1/p' init.sh)
[ -n "$FILES" ] || { echo "file list not found in init.sh"; exit 1; }
initialize() { for f in $FILES; do "${PSQL[@]}" < "$f" >/dev/null; done; }
pages() { "${PSQL[@]}" -c "SELECT pg_relation_size('user_search_norms') / current_setting('block_size')::int"; }

"${PSQL[@]}" < 001-schema.sql >/dev/null
# 初期データ: 境界 (003-initial-snapshot.sql が created_at の最大値で決める) より前の利用者 3000 人。
"${PSQL[@]}" -c "INSERT INTO users(id, username, secret_key_hash, display_name, region, created_at)
SELECT 'init-' || i, 'init' || i, 'x', 'Init ' || i, 'asia', timestamptz '2026-01-01' + i * interval '1 minute'
FROM generate_series(1, 3000) i" >/dev/null
"${PSQL[@]}" < 003-initial-snapshot.sql >/dev/null

initialize
# 起動時・initialize 後の refreshSearchNorms に相当: 全利用者の正規化を保存する。
NORM="INSERT INTO user_search_norms SELECT id, lower(username), display_name, lower(display_name) FROM users"
"${PSQL[@]}" -c "$NORM" >/dev/null

"${PSQL[@]}" >/dev/null <<SQL
-- 初期データの利用者の改名 (検索が正規化を保存し直す)。initialize で users は元に戻り、正規化は古いまま残る。
UPDATE users SET display_name = 'Renamed' WHERE id = 'init-1';
UPDATE user_search_norms SET display_name = 'Renamed', display_name_norm = 'renamed' WHERE user_id = 'init-1';
-- ベンチで登録された利用者 2000 人 (境界より後) とその正規化。
INSERT INTO users(id, username, secret_key_hash, display_name, region, created_at)
SELECT 'bench-' || i, 'bench' || i, 'x', 'Bench ' || i, 'europe',
       (SELECT created_at FROM initial_data_boundary) + i * interval '1 second'
FROM generate_series(1, 2000) i;
$NORM WHERE id LIKE 'bench-%';
-- 以前の initialize で消えた利用者の行 (修正前の reset.sql が残したもの)。
INSERT INTO user_search_norms SELECT 'gone-' || i, 'gone' || i, 'Gone ' || i, 'gone ' || i FROM generate_series(1, 4000) i;
SQL
BEFORE=$(pages)

initialize
AFTER=$(pages)
initialize
AGAIN=$(pages)

CHECK="SELECT (SELECT count(*) FROM users) || ' ' ||
  (SELECT count(*) FROM user_search_norms) || ' ' ||
  (SELECT count(*) FROM user_search_norms s WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = s.user_id)) || ' ' ||
  (SELECT count(*) FROM user_search_norms s JOIN initial_users i ON i.id = s.user_id) || ' ' ||
  (SELECT count(*) FROM users u JOIN user_search_norms s ON s.user_id = u.id WHERE s.display_name IS DISTINCT FROM u.display_name)"
read -r users norms orphans kept stale <<<"$("${PSQL[@]}" -c "$CHECK")"
echo "users=$users norms=$norms orphans=$orphans kept_initial=$kept stale=$stale"
echo "user_search_norms pages: before=$BEFORE after=$AFTER again=$AGAIN"

fail=0
[ "$users" = 3000 ] || { echo "  FAIL: users after the boundary were not removed"; fail=1; }
[ "$orphans" = 0 ] || { echo "  FAIL: user_search_norms keeps rows of removed users"; fail=1; }
[ "$kept" = 3000 ] || { echo "  FAIL: rows of initial users were removed"; fail=1; }
# init-1 の正規化は古いまま残り、initialize 後の refreshSearchNorms (アプリ) が保存し直す。
[ "$stale" = 1 ] || { echo "  FAIL: stale row of a renamed initial user was not kept"; fail=1; }
[ "$AFTER" -lt "$BEFORE" ] || { echo "  FAIL: user_search_norms did not shrink"; fail=1; }
[ "$AGAIN" -le "$AFTER" ] || { echo "  FAIL: user_search_norms grew on a repeated initialize"; fail=1; }
[ "$fail" = 0 ] && echo PASS || { echo FAIL; exit 1; }
