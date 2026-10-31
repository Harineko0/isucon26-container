-- 新しく投稿された画像の本体は、受け付けたリージョンのアプリサーバーのディスクに置く (webapp/go/image_store.go)。
-- その行は data を空にして、stored_region に置き場のリージョンを入れる。NULL の行 (初期データなど) は従来どおり data に本体がある。
-- 列の追加はカタログの更新だけ (NULL・既定値なしなので書き換えない)。すでにあれば何もしない (ロックも取らない)。
-- ロックが取れなければ initialize を失敗させる (列が無いまま動くと投稿がすべて失敗するため)。
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_attribute
    WHERE attrelid = 'image_objects'::regclass AND attname = 'stored_region' AND NOT attisdropped
  ) THEN
    SET LOCAL lock_timeout = '10s';
    ALTER TABLE image_objects ADD COLUMN stored_region text;
  END IF;
END $$;
