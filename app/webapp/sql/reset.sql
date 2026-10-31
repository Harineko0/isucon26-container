SET session_replication_role = replica;

BEGIN;

TRUNCATE ip_bans, login_sessions, administrator_sessions, friendships, friend_requests, follows, block_relations;

DELETE FROM notifications WHERE created_at > (SELECT created_at FROM initial_data_boundary);
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
