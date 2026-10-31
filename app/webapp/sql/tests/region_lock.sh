#!/bin/bash
# #79 (投稿期間の advisory lock) と #83 (isureal_update_me の status 条件) を、手元の Docker の PostgreSQL 16 で確かめる。
#   - 002〜008 を init.sh と同じ順で 2 回流しても失敗しないこと (initialize が毎回流す)
#   - 切替側と同じ排他の advisory lock を持つトランザクションがあるあいだ、isureal_create_post が
#     advisory lock で待ち (pg_locks)、posting_windows の行ロック (tuple / FOR SHARE) は取らないこと。
#     解放後に投稿が 'ok' で終わり、posted_at が新しい期間の starts_at 以降になること
#   - isureal_update_me が削除済み (status <> 'active') の利用者の行を書き換えず not_found を返すこと
# 使い方: app/webapp/sql/tests/region_lock.sh
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
NAME="isureal-region-lock-test-$$"
PSQL=(docker exec -i "$NAME" psql -U postgres -d postgres -v ON_ERROR_STOP=1 -qAtX)

docker run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=pw postgres:16 >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
until docker exec "$NAME" pg_isready -U postgres -q 2>/dev/null; do sleep 0.5; done
sleep 1

"${PSQL[@]}" < 001-schema.sql >/dev/null 2>&1
for pass in 1 2; do
	for f in 002-indexes.sql 007-image-location.sql 004-functions.sql 005-roundtrips.sql 006-image-storage.sql 007-feed-indexes.sql 008-image-partitions.sql; do  # init.sh と同じ順
		"${PSQL[@]}" < "$f" >/dev/null 2>&1 || { echo "FAIL: $f (pass $pass)"; exit 1; }
	done
done

"${PSQL[@]}" <<'SQL'
INSERT INTO users(id, username, secret_key_hash, display_name, region, status) VALUES
  ('u-active', 'u-active', 'x', 'A', 'asia', 'active'),
  ('u-deleted', 'u-deleted', 'x', 'D', 'asia', 'deleted');
INSERT INTO posting_windows(id, region, local_date, starts_at, on_time_deadline)
VALUES ('w-old', 'asia', '2026-11-01', now() - interval '1 hour', now() + interval '1 hour');
SQL

# --- #83
out=$("${PSQL[@]}" <<'SQL'
SELECT o_result FROM isureal_update_me('u-deleted', 'changed', NULL, NULL);
SELECT display_name FROM users WHERE id = 'u-deleted';
SELECT o_result || ':' || o_display_name FROM isureal_update_me('u-active', 'changed', NULL, NULL);
SQL
)
expected=$'not_found\nD\nok:changed'
if [ "$out" != "$expected" ]; then echo "FAIL: update_me"; echo "$out"; exit 1; fi
echo "ok: update_me leaves deleted users alone"

# --- #79: 切替側 (排他ロック) を持ったまま投稿を流す
hold=$(mktemp)
(
	docker exec -i "$NAME" psql -U postgres -d postgres -qAtX <<'SQL'
BEGIN;
SELECT pg_advisory_xact_lock(isureal_region_lock_key('asia'));
SELECT pg_sleep(3);
UPDATE posting_windows SET ended_at = clock_timestamp() WHERE id = 'w-old';
INSERT INTO posting_windows(id, region, local_date, starts_at, on_time_deadline)
VALUES ('w-new', 'asia', '2026-11-02', clock_timestamp(), clock_timestamp() + interval '1 hour');
COMMIT;
SELECT 'switched';
SQL
) >"$hold" 2>&1 &
sleep 1
post_out_file=$(mktemp)
# 背景で動かす側は -i を付けない (stdin が無いと docker exec -i が出力を落とすことがある)。
docker exec "$NAME" psql -U postgres -d postgres -v ON_ERROR_STOP=1 -qAtX \
  -c "SELECT o_result || ':' || o_window_id
  FROM isureal_create_post('u-active', 'img-1', 'post-1', '\\x00'::bytea, 'c', 'public', 'none', NULL, NULL, NULL, NULL)" >"$post_out_file" 2>&1 &
post_pid=$!
sleep 1
locks=$("${PSQL[@]}" -c "SELECT locktype || ':' || granted::text FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
  WHERE a.query LIKE '%isureal_create_post%' AND l.locktype IN ('advisory', 'tuple') ORDER BY 1")
wait "$post_pid" || true
post_out=$(cat "$post_out_file")
wait
grep -q switched "$hold" || { echo "FAIL: switch did not commit"; cat "$hold"; exit 1; }
echo "locks while waiting: $(tr '\n' ' ' <<<"$locks")"
case "$locks" in
  *advisory:f*) ;;
  *) echo "FAIL: create_post did not wait on the advisory lock"; exit 1;;
esac
case "$locks" in
  *tuple*) echo "FAIL: create_post still takes a tuple lock on posting_windows"; exit 1;;
esac
[ "$post_out" = "ok:w-new" ] || { echo "FAIL: create_post result '$post_out' (want ok:w-new)"; exit 1; }
# 投稿時刻は切替後に取り直され、新しい期間の starts_at 以降になる (別の文で確かめる: 呼び出し元の文のスナップショットは
# ロック待ちの前にとられていて新しい期間が見えないが、関数の中の文は文ごとに新しいスナップショットをとる)。
in_window=$("${PSQL[@]}" -c "SELECT (p.posted_at >= w.starts_at)::text FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id WHERE p.id = 'post-1'")
[ "$in_window" = "true" ] || { echo "FAIL: posted_at is before the new window's starts_at"; exit 1; }
echo "ok: create_post waited on the region advisory lock and landed in the new window"
rm -f "$hold" "$post_out_file"
