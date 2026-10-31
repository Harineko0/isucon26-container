CREATE INDEX IF NOT EXISTS login_sessions_token_hash ON login_sessions(token_hash);
CREATE INDEX IF NOT EXISTS login_sessions_user_id_active ON login_sessions(user_id) WHERE active;
CREATE INDEX IF NOT EXISTS administrator_sessions_token_hash ON administrator_sessions(token_hash);
DROP INDEX IF EXISTS users_username;
CREATE UNIQUE INDEX IF NOT EXISTS users_username_key ON users(username);
CREATE INDEX IF NOT EXISTS users_region_active ON users(region) WHERE status = 'active';
CREATE INDEX IF NOT EXISTS friendships_user_high_id ON friendships(user_high_id);
CREATE INDEX IF NOT EXISTS follows_followee_id ON follows(followee_id);
CREATE INDEX IF NOT EXISTS block_relations_blocked_id ON block_relations(blocked_id);
CREATE INDEX IF NOT EXISTS friend_requests_recipient_status ON friend_requests(recipient_id, status);
CREATE INDEX IF NOT EXISTS friend_requests_requester_status ON friend_requests(requester_id, status);
CREATE INDEX IF NOT EXISTS posts_image_id ON posts(image_id);
CREATE INDEX IF NOT EXISTS realmojis_image_id ON realmojis(image_id);
DROP INDEX IF EXISTS notifications_user_created;
CREATE INDEX IF NOT EXISTS posting_windows_region_active ON posting_windows(region) WHERE ended_at IS NULL;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_user_id_fkey;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_posting_window_id_fkey;
-- 通知は投稿期間ごとにまとめて作るので、(投稿期間, 利用者) の順の一意索引にして挿入先のページを局所化する。
CREATE UNIQUE INDEX IF NOT EXISTS notifications_window_user ON notifications(posting_window_id, user_id);
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_user_id_posting_window_id_key;
-- 利用者検索は正規化済みの名前 (user_search_norms) で SQL 側で照合する (user_read_api.go)。
-- 以前の trigram による絞り込みの索引は使わないので消す (書き込みの負荷を減らす)。
DROP INDEX IF EXISTS users_username_trgm, users_display_name_trgm, users_folds_to_ascii;
CREATE TABLE IF NOT EXISTS user_search_norms (user_id text PRIMARY KEY, username_norm text NOT NULL, display_name text NOT NULL, display_name_norm text NOT NULL);
