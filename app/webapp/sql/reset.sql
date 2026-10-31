SET session_replication_role = replica;

BEGIN;

TRUNCATE ip_bans, login_sessions, administrator_sessions, friendships, friend_requests, follows, block_relations;

-- 通知は投稿期間の開始時にだけ、その期間の posting_window_id 付きで作られる(NOT NULL)。
-- created_at に索引が無く全件走査になるので、境界より後の投稿期間から notifications_window_user で引く。
-- posting_windows の統計が古いと境界後の期間数を過大に見積もり（実測 18 を 369）、notifications の全件走査を選ぶ。
-- 1.1k 行の表なので ANALYZE は数 ms。これで notifications_window_user のインデックス経由になる。
ANALYZE posting_windows;
DELETE FROM notifications WHERE posting_window_id IN (SELECT id FROM posting_windows WHERE created_at > (SELECT created_at FROM initial_data_boundary));
DELETE FROM comments WHERE created_at > (SELECT created_at FROM initial_data_boundary);
DELETE FROM realmojis WHERE created_at > (SELECT created_at FROM initial_data_boundary);
DELETE FROM posts WHERE created_at > (SELECT created_at FROM initial_data_boundary);
DELETE FROM posting_windows WHERE created_at > (SELECT created_at FROM initial_data_boundary);
DO $$
DECLARE partition_name text;
BEGIN
  FOR partition_name IN
    SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
    WHERE i.inhparent = 'image_objects'::regclass
      AND c.relname > 'image_objects_' || to_char((SELECT created_at FROM initial_data_boundary) AT TIME ZONE 'UTC', 'YYYYMM')
  LOOP
    EXECUTE format('TRUNCATE %I', partition_name);
  END LOOP;
END $$;
DELETE FROM image_objects WHERE created_at > (SELECT created_at FROM initial_data_boundary);
DELETE FROM users WHERE created_at > (SELECT created_at FROM initial_data_boundary);
-- 利用者検索の正規化 (user_search_norms、go/user_read_api.go) は利用者ごとに 1 行あり、外部キーは無い。
-- 上で消した利用者の行も消す。消さないとベンチで登録された利用者の行が initialize のたびに溜まり、
-- 検索が読むページが増える。NOT EXISTS なので、以前の initialize で残った行もここで消える。
-- 表は 002-indexes.sql が作る (init.sh はこのファイルより先に流す)。
DELETE FROM user_search_norms s WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = s.user_id);

UPDATE users u
SET display_name = i.display_name, bio = i.bio, profile_image_id = i.profile_image_id, region = i.region,
    account_type = i.account_type, status = i.status, deleted_at = i.deleted_at, updated_at = i.updated_at
FROM initial_users i
WHERE u.id = i.id
  AND (u.display_name, u.bio, u.profile_image_id, u.region, u.account_type, u.status, u.deleted_at, u.updated_at)
      IS DISTINCT FROM (i.display_name, i.bio, i.profile_image_id, i.region, i.account_type, i.status, i.deleted_at, i.updated_at);
-- 上の UPDATE は session_replication_role = replica なので users_search_dirty_update トリガーが発火しない。
-- ベンチが変えた表示名を初期値に戻した利用者は user_search_norms が古い名前のままになり、initialize 後の
-- refreshSearchNorms (非同期) が終わるまで、初期の名前で検索しても見つからない。トリガーの代わりにここで dirty に入れる
-- (検索はアプリで照合する)。002-indexes.sql の末尾と同じ文で、5.6k 行の結合なので数 ms。
INSERT INTO user_search_dirty (user_id)
SELECT u.id FROM users u LEFT JOIN user_search_norms s ON s.user_id = u.id
WHERE s.display_name IS DISTINCT FROM u.display_name
ON CONFLICT DO NOTHING;

INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at)
SELECT id, requester_id, recipient_id, status, created_at, updated_at FROM initial_friend_requests;
INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id, created_at, updated_at)
SELECT id, user_low_id, user_high_id, source_request_id, created_at, updated_at FROM initial_friendships;
INSERT INTO follows (id, follower_id, followee_id, created_at, updated_at)
SELECT id, follower_id, followee_id, created_at, updated_at FROM initial_follows;
INSERT INTO block_relations (id, blocker_id, blocked_id, created_at)
SELECT id, blocker_id, blocked_id, created_at FROM initial_block_relations;

COMMIT;

-- 上で消した user_search_norms の行を回収し、空になった末尾のページを切り詰める。残る初期データの利用者の
-- 行は表の先頭に固まっている (2026-10-31 の asia2 で 293 ページ中 0〜79 ページ) ので、表が縮み、検索の
-- Seq Scan が読むページも減る。autovacuum に任せるとベンチ中になり、検索と競合して切り詰めを諦めることがある。
-- VACUUM はトランザクションの外でしか流せない。ほかの VACUUM が動いていれば待たずに飛ばす (SKIP_LOCKED)。
VACUUM (SKIP_LOCKED) user_search_norms;

-- friendships / friend_requests / follows / block_relations は上で TRUNCATE してから入れ直したので、可視性マップが
-- 空で統計も古い。autovacuum が回るまで（実測でベンチ開始から約55秒）Index Only Scan でも heap を読み、
-- プランナーも古い統計で計画する（friendships_high_low を入れた #122 の後、friends-of-friends のフィードが読む
-- ページが 269 → 482 に増えた）。合計約6.5万行の小さい表なので、ここで VACUUM (ANALYZE) しておく。
VACUUM (ANALYZE, SKIP_LOCKED) friendships, friend_requests, follows, block_relations;
