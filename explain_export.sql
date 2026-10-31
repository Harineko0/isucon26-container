\set uid '\'81d06ead-77f3-4bfc-a4fe-1af4fe58a67a\''
SELECT count(*) FROM posts WHERE author_id = :uid;
EXPLAIN (ANALYZE, BUFFERS) SELECT id, length(data) FROM image_objects WHERE id IN (SELECT image_id FROM posts WHERE author_id = :uid ORDER BY posted_at LIMIT 60 OFFSET 60);
EXPLAIN (ANALYZE, BUFFERS) SELECT i.id, length(i.data) FROM (SELECT image_id, posted_at FROM posts WHERE author_id = :uid ORDER BY posted_at LIMIT 60) k JOIN image_objects i ON i.id = k.image_id AND i.created_at = k.posted_at;
