-- image_objects.data は JPEG(圧縮できない)なので、TOAST の pglz 圧縮の試行を省く(STORAGE EXTERNAL)。
-- 既存の行は書き換えず、新しく入る行から効く(カタログの更新だけ)。パーティションにも伝播する。
-- ALTER TABLE は親と全パーティションに ACCESS EXCLUSIVE を取るので、すでに EXTERNAL なら何もしない。
-- ロックが取れないときは initialize を失敗させずに諦める(次の initialize で再試行される)。
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM pg_attribute a
    WHERE a.attname = 'data' AND a.attstorage <> 'e'
      AND (a.attrelid = 'image_objects'::regclass
           OR a.attrelid IN (SELECT inhrelid FROM pg_inherits WHERE inhparent = 'image_objects'::regclass))
  ) THEN
    SET LOCAL lock_timeout = '2s';
    ALTER TABLE image_objects ALTER COLUMN data SET STORAGE EXTERNAL;
  END IF;
EXCEPTION
  WHEN lock_not_available THEN
    RAISE NOTICE 'image_objects SET STORAGE EXTERNAL skipped: lock not available';
END $$;
