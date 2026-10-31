-- Keep each mutation and its locks in one transaction without inter-region round trips.
CREATE OR REPLACE FUNCTION isureal_delete_comment(p_actor text, p_comment text)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  v_author text;
  v_post_author text;
BEGIN
  PERFORM 1 FROM users WHERE id = p_actor FOR NO KEY UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'user not found'; END IF;
  SELECT c.author_id, p.author_id INTO v_author, v_post_author
  FROM comments c JOIN posts p ON p.id = c.post_id
  WHERE c.id = p_comment AND c.deleted_at IS NULL FOR UPDATE OF c;
  IF NOT FOUND THEN RETURN 'not_found'; END IF;
  IF p_actor <> v_author AND p_actor <> v_post_author THEN RETURN 'forbidden'; END IF;
  UPDATE comments SET deleted_at = clock_timestamp() WHERE id = p_comment;
  RETURN 'ok';
END $$;

CREATE OR REPLACE FUNCTION isureal_delete_realmoji(p_actor text, p_realmoji text)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  v_reactor text;
  v_author text;
BEGIN
  PERFORM 1 FROM users WHERE id = p_actor FOR NO KEY UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'user not found'; END IF;
  SELECT r.reactor_id, p.author_id INTO v_reactor, v_author
  FROM realmojis r JOIN posts p ON p.id = r.post_id
  WHERE r.id = p_realmoji AND r.deleted_at IS NULL FOR UPDATE OF r;
  IF NOT FOUND THEN RETURN 'not_found'; END IF;
  IF p_actor <> v_reactor AND p_actor <> v_author THEN RETURN 'forbidden'; END IF;
  UPDATE realmojis SET deleted_at = clock_timestamp() WHERE id = p_realmoji;
  RETURN 'ok';
END $$;

CREATE OR REPLACE FUNCTION isureal_transition_friend_request_authorized(
  p_actor text, p_request_id text, p_status text
) RETURNS TABLE (o_result text, o_id text, o_status text, o_updated_at timestamptz)
LANGUAGE plpgsql AS $$
DECLARE
  v_req friend_requests%ROWTYPE;
BEGIN
  SELECT * INTO v_req FROM friend_requests WHERE id = p_request_id;
  IF NOT FOUND THEN o_result := 'not_found'; RETURN NEXT; RETURN; END IF;
  IF p_status NOT IN ('accepted', 'rejected', 'cancelled') OR
     (p_status = 'cancelled' AND p_actor <> v_req.requester_id) OR
     (p_status <> 'cancelled' AND p_actor <> v_req.recipient_id) THEN
    o_result := 'forbidden'; RETURN NEXT; RETURN;
  END IF;
  RETURN QUERY SELECT * FROM isureal_transition_friend_request(
    v_req.id::text, v_req.requester_id::text, v_req.recipient_id::text, p_status);
END $$;
