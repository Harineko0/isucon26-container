-- 利用者表のアプリ内レプリカ (go/users_cache.go) のための版番号と変更ログ。
--
-- cache_versions.version は users への書き込み文ごとに、同じトランザクションの中で 1 ずつ増える (行ロックを取るので
-- 版の順序 = コミットの順序)。users_changes にはその版で変わった利用者の id を残す。アプリは 1 文で
-- 「今の版」と「覚えている版より後の変更」を同じスナップショットで読み、メモリの利用者表を DB と同じ状態にしてから
-- 検索する。版が変わらなければ変更は無い (すべての書き込みがトリガーを通るため)。
-- reset.sql (initialize) は session_replication_role = replica でトリガーを止めて users を書き換えるので、
-- 末尾で user_id = '*' の行 (全件読み直しの印) を入れる。
CREATE TABLE IF NOT EXISTS cache_versions (name text PRIMARY KEY, version bigint NOT NULL DEFAULT 0);
INSERT INTO cache_versions (name) VALUES ('users') ON CONFLICT DO NOTHING;
CREATE TABLE IF NOT EXISTS users_changes (version bigint NOT NULL, user_id varchar(36) NOT NULL);
CREATE INDEX IF NOT EXISTS users_changes_version ON users_changes (version);

CREATE OR REPLACE FUNCTION isureal_users_changed() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE v bigint;
BEGIN
  UPDATE cache_versions SET version = version + 1 WHERE name = 'users' RETURNING version INTO v;
  IF TG_OP = 'DELETE' THEN
    INSERT INTO users_changes (version, user_id) SELECT v, id FROM old_rows;
  ELSE
    INSERT INTO users_changes (version, user_id) SELECT v, id FROM new_rows;
  END IF;
  RETURN NULL;
END $$;
CREATE OR REPLACE TRIGGER users_changes_insert AFTER INSERT ON users
  REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION isureal_users_changed();
CREATE OR REPLACE TRIGGER users_changes_update AFTER UPDATE ON users
  REFERENCING NEW TABLE AS new_rows FOR EACH STATEMENT EXECUTE FUNCTION isureal_users_changed();
CREATE OR REPLACE TRIGGER users_changes_delete AFTER DELETE ON users
  REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT EXECUTE FUNCTION isureal_users_changed();
