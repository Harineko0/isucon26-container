-- ベンチ中の投稿画像は全部 image_objects_20261028 に入り、120 接続が同じ TOAST 表を伸ばそうとして
-- relation extension lock で直列化する(#81)。このパーティションを id のハッシュで 8 分割して TOAST 表を 8 つにする。
-- 中身は reset.sql が毎回 TRUNCATE するベンチのデータだけ(初期データは入っていない)なので、作り直して捨ててよい。
-- 名前は image_objects_20261028 のまま中間パーティションにするので、reset.sql の TRUNCATE はそのまま葉まで効く。
-- 主キー (id, created_at) は 2 段のパーティションキーを両方含むので親の主キー・外部キーがそのまま伝播する。
-- 一度分割したら何もしない。DETACH は親に ACCESS EXCLUSIVE を取るので、ロックが取れないときは
-- initialize を失敗させずに諦める(次の initialize で再試行される)。
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_partitioned_table WHERE partrelid = 'image_objects_20261028'::regclass) THEN
    SET LOCAL lock_timeout = '5s';
    ALTER TABLE image_objects DETACH PARTITION image_objects_20261028;
    ALTER TABLE image_objects_20261028 RENAME TO image_objects_20261028_old;
    ALTER INDEX image_objects_20261028_pkey RENAME TO image_objects_20261028_old_pkey;
    CREATE TABLE image_objects_20261028 PARTITION OF image_objects
      FOR VALUES FROM ('2026-10-28 06:00:00+00') TO ('2026-11-01 00:00:00+00')
      PARTITION BY HASH (id);
    FOR i IN 0..7 LOOP
      EXECUTE format('CREATE TABLE image_objects_20261028_p%s PARTITION OF image_objects_20261028 FOR VALUES WITH (MODULUS 8, REMAINDER %s)', i, i);
    END LOOP;
    -- 006 と同じく圧縮の試行を省く。新しい空の表なので即終わる。
    ALTER TABLE image_objects_20261028 ALTER COLUMN data SET STORAGE EXTERNAL;
    DROP TABLE image_objects_20261028_old;
  END IF;
EXCEPTION
  WHEN lock_not_available THEN
    RAISE NOTICE 'image_objects_20261028 hash partitioning skipped: lock not available';
END $$;
