#!/bin/bash
# #27 の isureal_notification_id を、手元の Docker の PostgreSQL 16 で確かめる。
#   - 投稿期間の開始処理の INSERT (admin_posting_window_api.go) と、SSE が投稿期間と利用者から組み立てる文
#     (notification_api.go) が、同じ利用者に同じ id・created_at を返すこと。対象は開始時点でその
#     リージョンにいる active な利用者だけであること
#   - id が 36 文字の UUID 形式 (version 4、variant 8〜b) で、先頭 13 文字が以前と同じ開始時刻 (ミリ秒) の
#     16 進であること、重複しないこと
#   - 004-functions.sql を何度流しても失敗しないこと (initialize が毎回流す)
# 使い方: app/webapp/sql/tests/notification_id.sh
set -euo pipefail
cd -- "$(dirname -- "$0")/.."
NAME="isureal-notification-id-test-$$"
PSQL=(docker exec -i "$NAME" psql -U postgres -d postgres -v ON_ERROR_STOP=1 -qAtX)

docker run -d --rm --name "$NAME" -e POSTGRES_PASSWORD=pw postgres:16 >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT
until docker exec "$NAME" pg_isready -U postgres -q 2>/dev/null; do sleep 0.5; done
sleep 1

"${PSQL[@]}" < 001-schema.sql >/dev/null 2>&1
"${PSQL[@]}" < 004-functions.sql >/dev/null 2>&1
"${PSQL[@]}" < 004-functions.sql >/dev/null 2>&1

out=$("${PSQL[@]}" <<'SQL'
INSERT INTO users(id, username, secret_key_hash, display_name, region, status)
SELECT gen_random_uuid()::text, 'user' || i, 'x', 'U', (ARRAY['asia','europe','america'])[1 + i % 3],
       CASE WHEN i % 10 = 0 THEN 'deleted' ELSE 'active' END
FROM generate_series(1, 3000) i;
INSERT INTO posting_windows(id, region, local_date, starts_at, on_time_deadline)
VALUES ('6f1c1a8e-0d6b-4b8a-9a43-2f3b0d6e1f00', 'asia', '2026-11-01',
        '2026-10-31 12:34:56.789123+09', '2026-11-01 12:34:56.789123+09');

-- 開始処理の INSERT (アプリと同じ文)
PREPARE ins(text, text, timestamptz) AS
   INSERT INTO notifications (id, user_id, posting_window_id, created_at)
   SELECT isureal_notification_id($2::text, users.id, $3::timestamptz), users.id, $2, $3
   FROM users
   WHERE region = $1 AND status = 'active';
EXECUTE ins('asia', '6f1c1a8e-0d6b-4b8a-9a43-2f3b0d6e1f00', '2026-10-31 12:34:56.789123+09');

-- SSE の文 (アプリと同じ文)。接続中の利用者として全員を渡す
PREPARE sse(text, text[]) AS
    SELECT u.id AS user_id, isureal_notification_id(w.id, u.id, w.starts_at) AS id, w.starts_at AS created_at,
           u.status = 'active' AS user_active, u.region AS user_region
    FROM posting_windows w JOIN users u ON u.id = ANY($2::text[])
    WHERE w.id = $1;
SELECT array_agg(id) AS all_users FROM users \gset
CREATE TEMP TABLE got AS EXECUTE sse('6f1c1a8e-0d6b-4b8a-9a43-2f3b0d6e1f00', :'all_users');

CREATE TEMP TABLE failures (what text);
-- 旧実装が発行した id は再計算しても同じであること。ミリ秒境界、負の epoch、
-- タイムゾーン、ASCII 以外の id、NULL を含めて時刻・ハッシュの全桁を比べる。
INSERT INTO failures
SELECT 'stored notification id changed'
FROM (VALUES
  ('w', 'u', '1970-01-01 00:00:00.000499+00', '00000000-0000-4d29-8683-83dd8a813f26'),
  ('w', 'u', '1970-01-01 00:00:00.000500+00', '00000000-0001-4d29-8683-83dd8a813f26'),
  ('w', 'u', '1969-12-31 23:59:59.999500+00', 'ffffffff-ffff-4d29-8683-83dd8a813f26'),
  ('投稿:期間', '利用者', '2026-10-31 23:59:59.999500+09', '01a19279-8580-46fc-8ace-a815aa178624'),
  ('', '', '2026-10-31 14:59:59.999500+00', '01a19279-8580-4853-9e90-f0351324bd73'),
  (NULL, 'u', 'epoch', NULL), ('w', NULL, 'epoch', NULL), ('w', 'u', NULL, NULL)
) AS fixture(w, u, started, expected)
WHERE isureal_notification_id(w, u, started::timestamptz) IS DISTINCT FROM expected;
INSERT INTO failures
SELECT 'no notifications inserted' WHERE NOT EXISTS (SELECT 1 FROM notifications);
INSERT INTO failures
SELECT 'bad format: ' || id FROM notifications
WHERE id !~ '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$';
INSERT INTO failures
SELECT 'duplicate ids' WHERE (SELECT count(DISTINCT id) FROM notifications) <> (SELECT count(*) FROM notifications);
INSERT INTO failures
SELECT 'variant ' || v || ' never used' FROM unnest(ARRAY['8','9','a','b']) v
WHERE NOT EXISTS (SELECT 1 FROM notifications WHERE substr(id, 20, 1) = v);
-- 以前の式の先頭 13 文字 (開始時刻のミリ秒)
INSERT INTO failures
SELECT 'time prefix differs: ' || n.id FROM notifications n,
  (SELECT lpad(to_hex((extract(epoch FROM '2026-10-31 12:34:56.789123+09'::timestamptz) * 1000)::bigint), 12, '0') AS ts) old
WHERE substr(n.id, 1, 13) <> substr(old.ts, 1, 8) || '-' || substr(old.ts, 9, 4);
INSERT INTO failures
SELECT 'sse/insert mismatch for ' || coalesce(g.user_id, n.user_id)
FROM got g FULL JOIN notifications n ON n.user_id = g.user_id
WHERE (g.user_active AND g.user_region = 'asia') IS DISTINCT FROM (n.id IS NOT NULL)
   OR (n.id IS NOT NULL AND (n.id <> g.id OR n.created_at <> g.created_at));

SELECT 'notifications: ' || count(*) || ', sse rows: ' || (SELECT count(*) FROM got) || ', sample id: ' || min(id)
FROM notifications;
SELECT 'FAIL ' || what FROM failures LIMIT 20;
SELECT CASE WHEN EXISTS (SELECT 1 FROM failures) THEN 'FAILED' ELSE 'OK' END;
SQL
)
echo "$out"
test "$(tail -n 1 <<<"$out")" = OK
