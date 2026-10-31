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
-- 著者の投稿は投稿期間ごとに 1 件なので、(著者, 投稿期間) で引けば Memories や当日投稿済みの判定が
-- 著者の全投稿 (最大 365 件、それぞれ別ページ) を読まずに済む。author_id 単独の索引はこれで代わる。
-- 検証: sql/tests/posts_author_window.sh
CREATE INDEX IF NOT EXISTS posts_author_window ON posts(author_id, posting_window_id);
DROP INDEX IF EXISTS posts_author_id;
CREATE INDEX IF NOT EXISTS realmojis_image_id ON realmojis(image_id);
-- reset.sql (initialize) の DELETE ... WHERE created_at > (初期データの境界) が毎回表を全件走査していた (#75)。
-- どの表も heap の並びが created_at 順 (pg_stats.correlation >= 0.9999) なので、BRIN なら末尾の範囲だけ読む。
-- 索引は数十 KB で INSERT 時の維持費はほぼ無い。初回は asia2 で CONCURRENTLY で作ってから入れる (ci/README.md #40)。
CREATE INDEX IF NOT EXISTS realmojis_created_brin ON realmojis USING brin (created_at);
CREATE INDEX IF NOT EXISTS posts_created_brin ON posts USING brin (created_at);
CREATE INDEX IF NOT EXISTS comments_created_brin ON comments USING brin (created_at);
DROP INDEX IF EXISTS notifications_user_created;
-- 001-schema.sql の posting_windows_one_active_region (UNIQUE (region) WHERE ended_at IS NULL) と同じ定義の
-- 索引を重ねて持っていたので消す (#78)。計画は UNIQUE 側で同じになる。
DROP INDEX IF EXISTS posting_windows_region_active;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_user_id_fkey;
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_posting_window_id_fkey;
-- 通知は投稿期間ごとにまとめて作るので、(投稿期間, 利用者) の順の一意索引にして挿入先のページを局所化する。
CREATE UNIQUE INDEX IF NOT EXISTS notifications_window_user ON notifications(posting_window_id, user_id);
ALTER TABLE notifications DROP CONSTRAINT IF EXISTS notifications_user_id_posting_window_id_key;
-- 利用者検索は正規化済みの名前 (user_search_norms) で SQL 側で照合する (user_read_api.go)。
-- 以前の trigram による絞り込みの索引は使わないので消す (書き込みの負荷を減らす)。
DROP INDEX IF EXISTS users_username_trgm, users_display_name_trgm, users_folds_to_ascii;
CREATE TABLE IF NOT EXISTS user_search_norms (user_id text PRIMARY KEY, username_norm text NOT NULL, display_name text NOT NULL, display_name_norm text NOT NULL);
-- 検索の候補は正規化済みの名前の trigram 索引で引き、全 active 利用者は並べ替えない (#26)。
-- user_search_dirty は名前が変わって正規化が追いついていない利用者 (検索はアプリで照合する)。
-- 以下は go/user_read_api.go の searchNormsSchema と同じ (起動時にも作る。変えるときは両方)。
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE INDEX IF NOT EXISTS user_search_norms_username_trgm ON user_search_norms USING gin (username_norm gin_trgm_ops);
CREATE INDEX IF NOT EXISTS user_search_norms_display_name_trgm ON user_search_norms USING gin (display_name_norm gin_trgm_ops);
CREATE TABLE IF NOT EXISTS user_search_dirty (user_id text PRIMARY KEY);
CREATE OR REPLACE FUNCTION isureal_mark_search_dirty() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO user_search_dirty (user_id) VALUES (NEW.id) ON CONFLICT DO NOTHING;
  RETURN NULL;
END $$;
CREATE OR REPLACE TRIGGER users_search_dirty_insert AFTER INSERT ON users
  FOR EACH ROW EXECUTE FUNCTION isureal_mark_search_dirty();
CREATE OR REPLACE TRIGGER users_search_dirty_update AFTER UPDATE OF display_name ON users
  FOR EACH ROW WHEN (OLD.display_name IS DISTINCT FROM NEW.display_name) EXECUTE FUNCTION isureal_mark_search_dirty();
INSERT INTO user_search_dirty (user_id)
  SELECT u.id FROM users u LEFT JOIN user_search_norms s ON s.user_id = u.id
  WHERE s.display_name IS DISTINCT FROM u.display_name
  ON CONFLICT DO NOTHING;
