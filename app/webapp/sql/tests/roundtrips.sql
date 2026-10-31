\set ON_ERROR_STOP on
BEGIN;
SET LOCAL statement_timeout = '15s';
DO $$
DECLARE
  a text := gen_random_uuid()::text;
  b text := gen_random_uuid()::text;
  outsider text := gen_random_uuid()::text;
  w text := gen_random_uuid()::text;
  p text := gen_random_uuid()::text;
  c text := gen_random_uuid()::text;
  r text := gen_random_uuid()::text;
  q text;
  s text;
  result text;
BEGIN
  INSERT INTO users(id, username, secret_key_hash, display_name, region)
  VALUES (a, a, 'test', 'a', 'europe'), (b, b, 'test', 'b', 'europe'),
         (outsider, outsider, 'test', 'outsider', 'europe');
  INSERT INTO posting_windows(id, region, local_date, starts_at, on_time_deadline, ended_at)
  VALUES(w, 'europe', '1900-01-01', now(), now(), now());
  INSERT INTO posts(id, author_id, posting_window_id, image_id, visibility, location_kind, posted_at)
  VALUES(p, a, w, gen_random_uuid()::text, 'public', 'none', now());
  INSERT INTO comments(id, post_id, author_id, body, created_at) VALUES(c, p, b, 'test', now());
  INSERT INTO realmojis(id, post_id, reactor_id, image_id, reaction_type, created_at, replaced_at)
  VALUES(r, p, b, gen_random_uuid()::text, 'like', now(), now());

  ASSERT isureal_delete_comment(outsider, c) = 'forbidden';
  ASSERT isureal_delete_realmoji(outsider, r) = 'forbidden';
  ASSERT (SELECT deleted_at IS NULL FROM comments WHERE id = c);
  ASSERT (SELECT deleted_at IS NULL FROM realmojis WHERE id = r);
  ASSERT isureal_delete_comment(b, gen_random_uuid()::text) = 'not_found';
  ASSERT isureal_delete_realmoji(b, gen_random_uuid()::text) = 'not_found';
  ASSERT isureal_delete_comment(b, c) = 'ok';
  ASSERT isureal_delete_realmoji(b, r) = 'ok';
  ASSERT (SELECT deleted_at IS NOT NULL FROM comments WHERE id = c);
  ASSERT (SELECT deleted_at IS NOT NULL FROM realmojis WHERE id = r);
  ASSERT isureal_delete_comment(b, c) = 'not_found';
  ASSERT isureal_delete_realmoji(b, r) = 'not_found';
  UPDATE comments SET deleted_at = NULL WHERE id = c;
  UPDATE realmojis SET deleted_at = NULL WHERE id = r;
  ASSERT isureal_delete_comment(a, c) = 'ok';
  ASSERT isureal_delete_realmoji(a, r) = 'ok';

  SELECT o_result INTO result FROM isureal_transition_friend_request_authorized(a, gen_random_uuid()::text, 'accepted');
  ASSERT result = 'not_found';
  FOREACH s IN ARRAY ARRAY['accepted', 'rejected', 'cancelled'] LOOP
    q := gen_random_uuid()::text;
    INSERT INTO friend_requests(id, requester_id, recipient_id, status, created_at, updated_at)
    VALUES(q, a, b, 'pending', now(), now());
    SELECT o_result INTO result FROM isureal_transition_friend_request_authorized(outsider, q, s);
    ASSERT result = 'forbidden';
    SELECT o_result INTO result FROM isureal_transition_friend_request_authorized(CASE WHEN s = 'cancelled' THEN b ELSE a END, q, s);
    ASSERT result = 'forbidden';
    ASSERT (SELECT status = 'pending' FROM friend_requests WHERE id = q);
    SELECT o_result INTO result FROM isureal_transition_friend_request_authorized(CASE WHEN s = 'cancelled' THEN a ELSE b END, q, s);
    ASSERT result = 'ok';
    ASSERT (SELECT status = s FROM friend_requests WHERE id = q);
    IF s = 'accepted' THEN
      ASSERT EXISTS(SELECT 1 FROM friendships WHERE user_low_id = least(a,b) AND user_high_id = greatest(a,b));
    END IF;
    SELECT o_result INTO result FROM isureal_transition_friend_request_authorized(CASE WHEN s = 'cancelled' THEN a ELSE b END, q, s);
    ASSERT result = 'forbidden';
  END LOOP;
END $$;
ROLLBACK;
