\set uid '\'81d06ead-77f3-4bfc-a4fe-1af4fe58a67a\''
EXPLAIN (ANALYZE, BUFFERS)
  WITH f AS (
    SELECT CASE WHEN user_low_id = :uid THEN user_high_id ELSE user_low_id END AS id
    FROM friendships WHERE user_low_id = :uid OR user_high_id = :uid
  )
  SELECT u.id,
    EXISTS(
      SELECT 1 FROM posts p
      JOIN posting_windows posted ON posted.id = p.posting_window_id
      JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                                  AND current.local_date = posted.local_date
      WHERE p.author_id = u.id AND p.deleted_at IS NULL
    ) AS has_posted_today,
    (SELECT string_agg(CASE WHEN blocker_id = :uid THEN blocked_id ELSE blocker_id END, ',')
     FROM block_relations WHERE blocker_id = :uid OR blocked_id = :uid) AS blocked,
    (SELECT string_agg(id, ',') FROM f) AS friends,
    (SELECT string_agg(pair, ',') FROM (
       SELECT fs.user_low_id || ':' || fs.user_high_id AS pair FROM friendships fs JOIN f ON fs.user_low_id = f.id
       UNION
       SELECT fs.user_low_id || ':' || fs.user_high_id AS pair FROM friendships fs JOIN f ON fs.user_high_id = f.id
     ) pairs) AS pairs
  FROM users u WHERE u.id = :uid AND u.status = 'active';
