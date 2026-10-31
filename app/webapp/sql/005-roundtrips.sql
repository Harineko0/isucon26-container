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

-- #17: RegionMiddleware の無い書き込み (logout, PATCH /api/me) も、セッションの確認と同じ 1 文で呼べるように関数にする
-- (Go 側は callWithSessionAnyRegion)。中身は Go のハンドラにあった文と同じ順序・同じロック。

-- InvalidateLoginSession と同じ: 利用者を FOR NO KEY UPDATE でロックしてから、そのトークンのセッションを無効にする。
CREATE OR REPLACE FUNCTION isureal_logout(p_user text, p_token_hash text)
RETURNS text LANGUAGE plpgsql AS $$
BEGIN
  PERFORM 1 FROM users WHERE id = p_user FOR NO KEY UPDATE;
  IF NOT FOUND THEN RETURN 'not_found'; END IF;
  UPDATE login_sessions SET active = false, invalidated_at = clock_timestamp(), reason = 'logout'
  WHERE user_id = p_user AND active = true AND token_hash = p_token_hash;
  RETURN 'ok';
END $$;

-- updateMeHandler の UPDATE ... RETURNING と同じ (NULL の引数は変更しない)。status が active でなくても
-- 行は返す (従来どおり呼び出し側が 404 にする)。
CREATE OR REPLACE FUNCTION isureal_update_me(p_user text, p_display_name text, p_bio text, p_region text)
RETURNS TABLE (
  o_result text, o_id text, o_username text, o_secret_key_hash text, o_display_name text, o_bio text,
  o_profile_image_id text, o_region text, o_account_type text, o_status text
) LANGUAGE plpgsql AS $$
BEGIN
  RETURN QUERY
    UPDATE users SET display_name = COALESCE(p_display_name, display_name), bio = COALESCE(p_bio, bio),
                     region = COALESCE(p_region, region), updated_at = clock_timestamp()
    WHERE id = p_user
    RETURNING 'ok'::text, id::text, username::text, secret_key_hash, display_name, bio,
              profile_image_id::text, region, account_type, status;
  IF NOT FOUND THEN o_result := 'not_found'; RETURN NEXT; END IF;
END $$;
