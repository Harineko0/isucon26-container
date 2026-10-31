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
  v_active boolean;
  v_status text;
  v_blocked boolean;
BEGIN
  SELECT * INTO v_req FROM friend_requests WHERE id = p_request_id;
  IF NOT FOUND THEN o_result := 'not_found'; RETURN NEXT; RETURN; END IF;
  IF p_status NOT IN ('accepted', 'rejected', 'cancelled') OR
     (p_status = 'cancelled' AND p_actor <> v_req.requester_id) OR
     (p_status <> 'cancelled' AND p_actor <> v_req.recipient_id) THEN
    o_result := 'forbidden'; RETURN NEXT; RETURN;
  END IF;
  -- 以下は isureal_transition_friend_request(v_req.id, v_req.requester_id, v_req.recipient_id, p_status) と同じ判定・
  -- 同じ順序のロックを、関数の入れ子(RETURN QUERY → lock_pair)なしで行う。#154 のベンチでこの経路は 1 回 22ms で、
  -- その多くは文の数(lock_pair の 4 文、読み直し、ブロックの確認、INSERT、UPDATE)だった。
  IF v_req.requester_id = v_req.recipient_id OR v_req.recipient_id = '' THEN o_result := 'bad'; RETURN NEXT; RETURN; END IF;
  PERFORM pg_advisory_xact_lock_shared(isureal_user_lock_key(least(v_req.requester_id, v_req.recipient_id))),
          pg_advisory_xact_lock_shared(isureal_user_lock_key(greatest(v_req.requester_id, v_req.recipient_id))),
          pg_advisory_xact_lock(isureal_pair_lock_key(v_req.requester_id, v_req.recipient_id));
  -- ロックの後の 1 文で、2 人とも active か・申請の今の status・(承認なら)ブロックの有無を読む。
  -- 申請とブロックの行を書くのは 2 人組のロックを持つ関数だけなので、別々の文で読んだときと同じ値になる。
  SELECT (SELECT count(*) FROM users WHERE id IN (v_req.requester_id, v_req.recipient_id) AND status = 'active') = 2,
         fr.status,
         CASE WHEN p_status = 'accepted' THEN EXISTS(
           SELECT 1 FROM block_relations WHERE blocker_id = v_req.requester_id AND blocked_id = v_req.recipient_id
           UNION ALL
           SELECT 1 FROM block_relations WHERE blocker_id = v_req.recipient_id AND blocked_id = v_req.requester_id
         ) ELSE false END
    INTO v_active, v_status, v_blocked
    FROM friend_requests fr WHERE fr.id = v_req.id;
  IF NOT v_active THEN o_result := 'not_found'; RETURN NEXT; RETURN; END IF;
  IF v_status <> 'pending' THEN o_result := 'forbidden'; RETURN NEXT; RETURN; END IF;
  IF v_blocked THEN o_result := 'conflict'; RETURN NEXT; RETURN; END IF;
  -- 承認なら友達の行も足す(以前は INSERT と UPDATE の 2 文。どちらかが失敗すれば文ごと失敗するのは同じ)。
  WITH upd AS (
    UPDATE friend_requests SET status = p_status, updated_at = clock_timestamp() WHERE id = v_req.id
    RETURNING id, status, updated_at
  ), ins AS (
    INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id)
    SELECT gen_random_uuid()::text, least(v_req.requester_id, v_req.recipient_id), greatest(v_req.requester_id, v_req.recipient_id), v_req.id
    WHERE p_status = 'accepted'
  )
  SELECT id, status, updated_at INTO o_id, o_status, o_updated_at FROM upd;
  IF NOT FOUND THEN RETURN; END IF;
  o_result := 'ok'; RETURN NEXT;
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

-- #17: PATCH /api/posts/:postId を「セッション確認 → isureal_update_post → 投稿と投稿者の再取得」の 3 往復から
-- 1 文にする (Go 側は callWithSessionColumns)。更新は isureal_update_post そのもの (同じ順序・同じロック)。
-- 'ok' のときだけ、更新後の投稿と投稿者を getPostWithAuthor と同じ条件で返す。plpgsql (VOLATILE) は文ごとに
-- 新しいスナップショットを取るので、同じトランザクションで更新した後の値が読める。それ以外は o_result だけの 1 行。
CREATE OR REPLACE FUNCTION isureal_update_post_returning(p_user text, p_post text, p_caption text, p_visibility text)
RETURNS TABLE (
  o_result text, o_id text, o_author_id text, o_posting_window_id text, o_image_id text, o_caption text,
  o_visibility text, o_location_kind text, o_latitude numeric, o_longitude numeric, o_locality text,
  o_posted_at timestamptz, o_on_time_deadline timestamptz, o_window_ended_at timestamptz,
  o_user_id text, o_username text, o_secret_key_hash text, o_display_name text, o_bio text,
  o_profile_image_id text, o_region text, o_account_type text, o_status text
) LANGUAGE plpgsql AS $$
DECLARE
  v_result text;
BEGIN
  v_result := isureal_update_post(p_user, p_post, p_caption, p_visibility);
  IF v_result = 'ok' THEN
    RETURN QUERY
      SELECT v_result, p.id::text, p.author_id::text, p.posting_window_id::text, p.image_id::text, p.caption,
             p.visibility, p.location_kind, p.latitude, p.longitude, p.locality,
             p.posted_at, w.on_time_deadline, w.ended_at,
             u.id::text, u.username::text, u.secret_key_hash, u.display_name, u.bio,
             u.profile_image_id::text, u.region, u.account_type, u.status
      FROM posts p
      JOIN posting_windows w ON w.id = p.posting_window_id
      JOIN users u ON u.id = p.author_id AND u.status = 'active'
      WHERE p.id = p_post AND p.deleted_at IS NULL;
    IF FOUND THEN RETURN; END IF;
  END IF;
  o_result := v_result;
  RETURN NEXT;
END $$;
