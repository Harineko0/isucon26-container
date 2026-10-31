-- リージョン間は 1 往復 100ms かかるので、複数文のトランザクションを関数にして 1 往復にする。
-- 中身はアプリにあった処理と同じ順序・同じロックで実行する。

-- 投稿時刻 (posted_at) はアプリの時計ではなく、この関数の中で DB の時計 (clock_timestamp()) から決めて返す。
-- 投稿期間の starts_at / ended_at / on_time_deadline も DB の時計なので、ホスト間の時計ずれが
-- onTime 判定や「p_posted_at < starts_at → 409」に影響しない。
-- 引数と戻り値が変わったので、古い定義 (p_posted_at 付き) は消してから作る (CREATE OR REPLACE では戻り値を変えられない)。
DROP FUNCTION IF EXISTS isureal_create_post(text, text, text, bytea, text, text, text, numeric, numeric, text, timestamptz);
DROP FUNCTION IF EXISTS isureal_create_post(text, text, text, bytea, text, text, text, numeric, numeric, text);

CREATE FUNCTION isureal_create_post(
  p_user_id text, p_image_id text, p_post_id text, p_data bytea, p_caption text, p_visibility text,
  p_location_kind text, p_latitude numeric, p_longitude numeric, p_locality text
) RETURNS TABLE (
  o_result text, o_window_id text, o_on_time_deadline timestamptz, o_posted_at timestamptz,
  o_latitude double precision, o_longitude double precision, o_locality text,
  o_id text, o_username text, o_secret_key_hash text, o_display_name text, o_bio text,
  o_profile_image_id text, o_region text, o_account_type text, o_status text
) LANGUAGE plpgsql AS $$
DECLARE
  u users%ROWTYPE;
  w posting_windows%ROWTYPE;
  n_active int;
  n_deleted int;
  v_fail text;
  v_image_at timestamptz;
  v_posted_at timestamptz;
BEGIN
  -- 画像の INSERT (数百 KB、数百 ms) はロックを取る前に済ませ、ロックの保持時間を短くする。
  -- 失敗の結果を返すときは IR001 を投げてこのブロック(サブトランザクション)ごと巻き戻すので、
  -- 画像が孤児として残ることはない(関数は正常終了してコミットされるため、巻き戻しが必要)。
  -- 画像の created_at は投稿時刻と同じ値にする (画像取得で (id, created_at) からパーティションを絞る手がかり)。
  -- まず今の時刻で画像を入れ、ロックを取った後に投稿期間の切り替わりをまたいでいたと分かった場合だけ、
  -- 投稿時刻を取り直して画像の created_at も揃える (下を参照)。
  BEGIN
    v_image_at := clock_timestamp();
    v_posted_at := v_image_at;
    -- 利用者が存在しなければ外部キー違反(23503) → 'no_user'(従来と同じ結果)。
    INSERT INTO image_objects (id, owner_user_id, data, media_type, created_at)
    VALUES (p_image_id, p_user_id, p_data, 'image/jpeg', v_image_at);
    SELECT * INTO u FROM users WHERE id = p_user_id AND status = 'active' FOR NO KEY UPDATE;
    IF NOT FOUND THEN
      v_fail := 'no_user'; RAISE SQLSTATE 'IR001';
    END IF;
    -- FOR SHARE: 同じリージョンの投稿どうしは並行させ、投稿期間の終了(UPDATE)とだけ排他にする。
    SELECT * INTO w FROM posting_windows WHERE region = u.region AND ended_at IS NULL FOR SHARE;
    IF NOT FOUND THEN
      SELECT * INTO w FROM posting_windows WHERE region = u.region AND ended_at IS NULL FOR SHARE;
    END IF;
    IF NOT FOUND THEN
      v_fail := 'slot_unavailable'; RAISE SQLSTATE 'IR001';
    END IF;
    SELECT count(*) FILTER (WHERE p.deleted_at IS NULL), count(*) FILTER (WHERE p.deleted_at IS NOT NULL)
    INTO n_active, n_deleted
    FROM posts p JOIN posting_windows pw ON pw.id = p.posting_window_id
    WHERE p.author_id = p_user_id AND pw.local_date = w.local_date;
    IF n_active <> 0 OR n_deleted >= 2 THEN
      v_fail := 'slot_unavailable'; RAISE SQLSTATE 'IR001';
    END IF;
    -- 画像を入れてからロックを取るまでの間に投稿期間が切り替わった (旧期間の ended_at = 新期間の starts_at は
    -- 切替側が FOR UPDATE を取った後に刻むので、v_posted_at がそれより前になる) 場合は、
    -- 今ロックを取っている新期間の中の時刻として投稿時刻を取り直す。切替は(新期間の)ロック取得より前に
    -- コミット済みなので、ここで取る clock_timestamp() は必ず starts_at 以降になる。
    -- 以前はここで 409 (slot_unavailable) にしていたが、切替待ちの間に届いた投稿を落とすだけだった。
    IF v_posted_at < w.starts_at THEN
      v_posted_at := clock_timestamp();
      UPDATE image_objects SET created_at = v_posted_at WHERE id = p_image_id AND created_at = v_image_at;
    END IF;
  EXCEPTION
    WHEN SQLSTATE 'IR001' THEN
      o_result := v_fail; RETURN NEXT; RETURN;
    WHEN foreign_key_violation THEN
      o_result := 'no_user'; RETURN NEXT; RETURN;
  END;
  INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility,
                     location_kind, latitude, longitude, locality, posted_at)
  VALUES (p_post_id, p_user_id, w.id, p_image_id, p_caption, p_visibility,
          p_location_kind, p_latitude, p_longitude, p_locality, v_posted_at)
  RETURNING 'ok', w.id, w.on_time_deadline, posts.posted_at, posts.latitude::double precision, posts.longitude::double precision, posts.locality,
         u.id, u.username, u.secret_key_hash, u.display_name, u.bio, u.profile_image_id, u.region, u.account_type, u.status
  INTO o_result, o_window_id, o_on_time_deadline, o_posted_at, o_latitude, o_longitude, o_locality,
       o_id, o_username, o_secret_key_hash, o_display_name, o_bio, o_profile_image_id, o_region, o_account_type, o_status;
  RETURN NEXT;
END $$;

CREATE OR REPLACE FUNCTION isureal_login(
  p_username text, p_secret_key_hash text, p_session_id text, p_token_hash text
) RETURNS TABLE (
  o_result text, o_id text, o_username text, o_display_name text, o_bio text,
  o_region text, o_account_type text, o_status text
) LANGUAGE plpgsql AS $$
DECLARE
  u users%ROWTYPE;
BEGIN
  SELECT * INTO u FROM users WHERE username = p_username FOR NO KEY UPDATE;
  IF NOT FOUND OR u.status <> 'active' OR u.secret_key_hash <> p_secret_key_hash THEN
    o_result := 'invalid'; RETURN NEXT; RETURN;
  END IF;
  UPDATE login_sessions
  SET active = false, invalidated_at = clock_timestamp(), reason = 'replaced'
  WHERE user_id = u.id AND active = true;
  INSERT INTO login_sessions (id, user_id, token_hash) VALUES (p_session_id, u.id, p_token_hash);
  SELECT 'ok', u.id, u.username, u.display_name, u.bio, u.region, u.account_type, u.status
  INTO o_result, o_id, o_username, o_display_name, o_bio, o_region, o_account_type, o_status;
  RETURN NEXT;
END $$;

CREATE OR REPLACE FUNCTION isureal_register(
  p_user_id text, p_username text, p_secret_key_hash text, p_display_name text, p_region text
) RETURNS TABLE (
  o_result text, o_id text, o_username text, o_display_name text, o_bio text,
  o_region text, o_account_type text, o_status text
) LANGUAGE plpgsql AS $$
BEGIN
  -- users_username_key(UNIQUE)で一意性を保証するので、テーブルロックは取らない。
  INSERT INTO users (id, username, secret_key_hash, display_name, region)
  VALUES (p_user_id, p_username, p_secret_key_hash, p_display_name, p_region)
  ON CONFLICT (username) DO NOTHING
  RETURNING 'ok', id::text, username::text, display_name, bio, region, account_type, status
  INTO o_result, o_id, o_username, o_display_name, o_bio, o_region, o_account_type, o_status;
  IF NOT FOUND THEN
    o_result := 'username_taken'; RETURN NEXT; RETURN;
  END IF;
  RETURN NEXT;
END $$;

-- 2 人の関係(友達申請・友達・フォロー・ブロック)の変更は、2 人組(順不同)の advisory lock で直列化する。
-- 以前は 2 人の users 行を FOR NO KEY UPDATE でロックしていたが、その行ロックは投稿・コメント・RealMoji・
-- ログイン・プロフィール画像などもとるので、無関係な操作(投稿期間の切り替わりを待っている投稿など)の後ろで待っていた。
-- 関係の変更が users 行から読むのは status(active か)と account_type だけで、status を変えるのは退会だけなので、
-- 利用者ごとの advisory lock を共有モードでとり、退会(isureal_delete_account)は同じ鍵を排他モードでとる。
-- 関係の表(friend_requests / friendships / follows / block_relations)を書くのは、2 人組のロックを持つ
-- isureal_relationship と isureal_transition_friend_request だけ。
-- ロックの順序: 利用者ごと(id 順) → 2 人組 → 行。コメント・RealMoji は行 → 2 人組だが、利用者ごとのロックはとらない。
CREATE OR REPLACE FUNCTION isureal_user_lock_key(p_user text) RETURNS bigint
LANGUAGE sql IMMUTABLE AS $$ SELECT hashtextextended('isureal:user:' || p_user, 0) $$;

CREATE OR REPLACE FUNCTION isureal_pair_lock_key(p_a text, p_b text) RETURNS bigint
LANGUAGE sql IMMUTABLE AS $$ SELECT hashtextextended('isureal:pair:' || least(p_a, p_b) || ':' || greatest(p_a, p_b), 0) $$;

-- 2 人をロックし、2 人とも active なら true を返す。
CREATE OR REPLACE FUNCTION isureal_lock_pair(p_a text, p_b text) RETURNS boolean
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_advisory_xact_lock_shared(isureal_user_lock_key(least(p_a, p_b)));
  PERFORM pg_advisory_xact_lock_shared(isureal_user_lock_key(greatest(p_a, p_b)));
  PERFORM pg_advisory_xact_lock(isureal_pair_lock_key(p_a, p_b));
  -- ロックをとった後の文なので、待っている間にコミットされた退会や関係の変更も見える
  -- (READ COMMITTED では VOLATILE な関数の文ごとに新しいスナップショットをとる)。
  RETURN (SELECT count(*) FROM users WHERE id IN (p_a, p_b) AND status = 'active') = 2;
END $$;

-- 友達申請・フォロー・ブロック。2 人組をロックしてから処理する。
-- o_result: ok / not_found(404) / conflict(409) / forbidden(403)
CREATE OR REPLACE FUNCTION isureal_relationship(
  p_action text, p_actor text, p_target text, p_new_id text
) RETURNS TABLE (o_result text, o_id text, o_status text, o_updated_at timestamptz)
LANGUAGE plpgsql AS $$
DECLARE
  v_low text := least(p_actor, p_target);
  v_high text := greatest(p_actor, p_target);
  v_type text;
  v_req friend_requests%ROWTYPE;
  v_count int;
BEGIN
  IF NOT isureal_lock_pair(p_actor, p_target) THEN o_result := 'not_found'; RETURN NEXT; RETURN; END IF;

  IF p_action = 'friend_request' THEN
    IF EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id = p_actor AND blocked_id = p_target) OR (blocker_id = p_target AND blocked_id = p_actor))
       OR EXISTS(SELECT 1 FROM friendships WHERE user_low_id = v_low AND user_high_id = v_high) THEN
      o_result := 'conflict'; RETURN NEXT; RETURN;
    END IF;
    SELECT * INTO v_req FROM friend_requests WHERE status = 'pending' AND
      ((requester_id = p_actor AND recipient_id = p_target) OR (requester_id = p_target AND recipient_id = p_actor))
    LIMIT 1;
    IF FOUND THEN
      IF v_req.requester_id = p_actor THEN o_result := 'conflict'; RETURN NEXT; RETURN; END IF;
      UPDATE friend_requests SET status = 'accepted', updated_at = clock_timestamp() WHERE id = v_req.id RETURNING * INTO v_req;
      INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id)
      VALUES (gen_random_uuid()::text, least(v_req.requester_id, v_req.recipient_id), greatest(v_req.requester_id, v_req.recipient_id), v_req.id);
    ELSE
      INSERT INTO friend_requests (id, requester_id, recipient_id, status, created_at, updated_at)
      VALUES (p_new_id, p_actor, p_target, 'pending', clock_timestamp(), clock_timestamp()) RETURNING * INTO v_req;
    END IF;
    RETURN QUERY SELECT 'ok'::text, v_req.id::text, v_req.status, v_req.updated_at;
    RETURN;
  ELSIF p_action = 'follow' THEN
    SELECT account_type INTO v_type FROM users WHERE id = p_target AND status = 'active';
    IF v_type IS DISTINCT FROM 'certified_public'
       OR EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id = p_actor AND blocked_id = p_target) OR (blocker_id = p_target AND blocked_id = p_actor))
       OR EXISTS(SELECT 1 FROM follows WHERE follower_id = p_actor AND followee_id = p_target) THEN
      o_result := 'conflict'; RETURN NEXT; RETURN;
    END IF;
    INSERT INTO follows (id, follower_id, followee_id) VALUES (p_new_id, p_actor, p_target);
  ELSIF p_action = 'unfollow' THEN
    DELETE FROM follows WHERE follower_id = p_actor AND followee_id = p_target;
    GET DIAGNOSTICS v_count = ROW_COUNT;
    IF v_count = 0 THEN o_result := 'conflict'; RETURN NEXT; RETURN; END IF;
  ELSIF p_action = 'unfriend' THEN
    DELETE FROM friendships WHERE user_low_id = v_low AND user_high_id = v_high;
    GET DIAGNOSTICS v_count = ROW_COUNT;
    IF v_count = 0 THEN o_result := 'conflict'; RETURN NEXT; RETURN; END IF;
  ELSIF p_action = 'block' THEN
    IF EXISTS(SELECT 1 FROM block_relations WHERE blocker_id = p_actor AND blocked_id = p_target) THEN
      o_result := 'conflict'; RETURN NEXT; RETURN;
    END IF;
    INSERT INTO block_relations (id, blocker_id, blocked_id) VALUES (p_new_id, p_actor, p_target);
    DELETE FROM friendships WHERE user_low_id = v_low AND user_high_id = v_high;
    DELETE FROM follows WHERE (follower_id = p_actor AND followee_id = p_target) OR (follower_id = p_target AND followee_id = p_actor);
    UPDATE friend_requests SET status = 'cancelled', updated_at = clock_timestamp()
    WHERE status = 'pending' AND ((requester_id = p_actor AND recipient_id = p_target) OR (requester_id = p_target AND recipient_id = p_actor));
  ELSIF p_action = 'unblock' THEN
    DELETE FROM block_relations WHERE blocker_id = p_actor AND blocked_id = p_target;
    GET DIAGNOSTICS v_count = ROW_COUNT;
    IF v_count = 0 THEN o_result := 'forbidden'; RETURN NEXT; RETURN; END IF;
  ELSE
    RAISE EXCEPTION 'unknown relationship action %', p_action;
  END IF;
  o_result := 'ok'; RETURN NEXT;
END $$;

CREATE OR REPLACE FUNCTION isureal_transition_friend_request(
  p_request_id text, p_requester text, p_recipient text, p_status text
) RETURNS TABLE (o_result text, o_id text, o_status text, o_updated_at timestamptz)
LANGUAGE plpgsql AS $$
DECLARE
  v_req friend_requests%ROWTYPE;
BEGIN
  IF p_requester = p_recipient OR p_recipient = '' THEN o_result := 'bad'; RETURN NEXT; RETURN; END IF;
  IF NOT isureal_lock_pair(p_requester, p_recipient) THEN o_result := 'not_found'; RETURN NEXT; RETURN; END IF;
  SELECT * INTO v_req FROM friend_requests WHERE id = p_request_id;
  IF v_req.status <> 'pending' THEN o_result := 'forbidden'; RETURN NEXT; RETURN; END IF;
  IF p_status = 'accepted' THEN
    IF EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id = v_req.requester_id AND blocked_id = v_req.recipient_id) OR (blocker_id = v_req.recipient_id AND blocked_id = v_req.requester_id)) THEN
      o_result := 'conflict'; RETURN NEXT; RETURN;
    END IF;
    INSERT INTO friendships (id, user_low_id, user_high_id, source_request_id)
    VALUES (gen_random_uuid()::text, least(v_req.requester_id, v_req.recipient_id), greatest(v_req.requester_id, v_req.recipient_id), v_req.id);
  END IF;
  RETURN QUERY UPDATE friend_requests SET status = p_status, updated_at = clock_timestamp() WHERE id = v_req.id
    RETURNING 'ok'::text, id::text, status, updated_at;
END $$;

-- viewerContext.canView / canComment と同じ判定。
CREATE OR REPLACE FUNCTION isureal_post_access(p_viewer text, p_post text, OUT can_view boolean, OUT can_comment boolean)
LANGUAGE plpgsql STABLE AS $$
DECLARE
  v users%ROWTYPE;
  p posts%ROWTYPE;
  w_ended timestamptz;
  posted boolean;
  is_friend boolean;
BEGIN
  can_view := false;
  can_comment := false;
  SELECT pp.* INTO p FROM posts pp JOIN users a ON a.id = pp.author_id AND a.status = 'active'
  WHERE pp.id = p_post AND pp.deleted_at IS NULL;
  IF NOT FOUND THEN RETURN; END IF;
  SELECT ended_at INTO w_ended FROM posting_windows WHERE id = p.posting_window_id;
  SELECT * INTO v FROM users WHERE id = p_viewer AND status = 'active';
  IF NOT FOUND THEN RETURN; END IF;
  IF w_ended IS NOT NULL THEN
    can_view := p.author_id = v.id;
    RETURN;
  END IF;
  posted := EXISTS(
    SELECT 1 FROM posts pp
    JOIN posting_windows pw ON pw.id = pp.posting_window_id
    JOIN posting_windows cur ON cur.region = v.region AND cur.ended_at IS NULL AND cur.local_date = pw.local_date
    WHERE pp.author_id = v.id AND pp.deleted_at IS NULL);
  IF p.author_id = v.id THEN
    can_view := true;
  ELSIF w_ended IS NOT NULL OR NOT posted
     OR EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id = v.id AND blocked_id = p.author_id) OR (blocker_id = p.author_id AND blocked_id = v.id)) THEN
    can_view := false;
  ELSIF p.visibility = 'public' THEN
    can_view := true;
  ELSE
    is_friend := EXISTS(SELECT 1 FROM friendships WHERE user_low_id = least(v.id, p.author_id) AND user_high_id = greatest(v.id, p.author_id));
    IF p.visibility = 'friends' THEN
      can_view := is_friend;
    ELSIF p.visibility = 'friends_of_friends' THEN
      can_view := is_friend OR EXISTS(
      SELECT 1 FROM friendships f1
      CROSS JOIN LATERAL (SELECT CASE WHEN f1.user_low_id = v.id THEN f1.user_high_id ELSE f1.user_low_id END AS mid) m
      JOIN friendships f2 ON (f2.user_low_id = m.mid AND f2.user_high_id = p.author_id) OR (f2.user_high_id = m.mid AND f2.user_low_id = p.author_id)
      WHERE f1.user_low_id = v.id OR f1.user_high_id = v.id);
    END IF;
  END IF;
  IF can_view AND w_ended IS NULL THEN
    IF v.id = p.author_id THEN
      can_comment := posted;
    ELSIF p.visibility = 'public' THEN
      can_comment := true;
    ELSE
      can_comment := is_friend;
    END IF;
  END IF;
END $$;

-- o_result: ok / forbidden(403 interaction_forbidden) / not_found(404 relationship_not_found)
CREATE OR REPLACE FUNCTION isureal_replace_realmoji(
  p_actor text, p_post text, p_id text, p_image_id text, p_data bytea, p_reaction_type text
) RETURNS TABLE (
  o_result text, o_created_at timestamptz,
  o_id text, o_username text, o_secret_key_hash text, o_display_name text, o_bio text,
  o_profile_image_id text, o_region text, o_account_type text, o_status text
) LANGUAGE plpgsql AS $$
DECLARE
  u users%ROWTYPE;
  p_author text;
  v_fail text;
BEGIN
  SELECT author_id INTO p_author FROM posts WHERE id = p_post AND deleted_at IS NULL;
  IF NOT FOUND OR p_author = p_actor THEN o_result := 'forbidden'; RETURN NEXT; RETURN; END IF;
  -- 画像の INSERT は 2 人分のロックを取る前に済ませる。失敗の結果はブロックごと巻き戻す
  -- (isureal_create_post と同じ。画像を孤児にしない)。
  BEGIN
    -- リアクションした人が存在しなければ外部キー違反(23503) → 'not_found'(従来と同じ結果)。
    INSERT INTO image_objects (id, owner_user_id, data, media_type) VALUES (p_image_id, p_actor, p_data, 'image/jpeg');
    PERFORM 1 FROM users WHERE id = least(p_actor, p_author) AND status = 'active' FOR NO KEY UPDATE;
    IF NOT FOUND THEN v_fail := 'not_found'; RAISE SQLSTATE 'IR001'; END IF;
    PERFORM 1 FROM users WHERE id = greatest(p_actor, p_author) AND status = 'active' FOR NO KEY UPDATE;
    IF NOT FOUND THEN v_fail := 'not_found'; RAISE SQLSTATE 'IR001'; END IF;
    -- 2 人の関係の変更(ブロック・友達の解除など)と直列化する(isureal_lock_pair)。
    PERFORM pg_advisory_xact_lock(isureal_pair_lock_key(p_actor, p_author));
    IF NOT (SELECT can_view FROM isureal_post_access(p_actor, p_post)) THEN
      v_fail := 'forbidden'; RAISE SQLSTATE 'IR001';
    END IF;
  EXCEPTION
    WHEN SQLSTATE 'IR001' THEN
      o_result := v_fail; RETURN NEXT; RETURN;
    WHEN foreign_key_violation THEN
      o_result := 'not_found'; RETURN NEXT; RETURN;
  END;
  UPDATE realmojis SET replaced_at = clock_timestamp()
  WHERE post_id = p_post AND reactor_id = p_actor AND replaced_at IS NULL AND deleted_at IS NULL;
  INSERT INTO realmojis (id, post_id, reactor_id, image_id, reaction_type, created_at)
  VALUES (p_id, p_post, p_actor, p_image_id, p_reaction_type, clock_timestamp()) RETURNING created_at INTO o_created_at;
  SELECT * INTO u FROM users WHERE id = p_actor AND status = 'active';
  IF NOT FOUND THEN RAISE EXCEPTION 'reactor not found'; END IF;
  RETURN QUERY SELECT 'ok'::text, o_created_at, u.id::text, u.username::text, u.secret_key_hash, u.display_name, u.bio,
    u.profile_image_id::text, u.region, u.account_type, u.status;
END $$;

-- o_result: ok / forbidden(403 interaction_forbidden)
CREATE OR REPLACE FUNCTION isureal_create_comment(
  p_actor text, p_post text, p_id text, p_body text
) RETURNS TABLE (
  o_result text, o_created_at timestamptz,
  o_id text, o_username text, o_secret_key_hash text, o_display_name text, o_bio text,
  o_profile_image_id text, o_region text, o_account_type text, o_status text
) LANGUAGE plpgsql AS $$
DECLARE
  u users%ROWTYPE;
  p_author text;
BEGIN
  SELECT author_id INTO p_author FROM posts WHERE id = p_post AND deleted_at IS NULL;
  IF NOT FOUND THEN o_result := 'forbidden'; RETURN NEXT; RETURN; END IF;
  PERFORM 1 FROM users WHERE id = least(p_actor, p_author) FOR NO KEY UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'user not found'; END IF;
  IF p_actor <> p_author THEN
    PERFORM 1 FROM users WHERE id = greatest(p_actor, p_author) FOR NO KEY UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'user not found'; END IF;
    -- 2 人の関係の変更(ブロック・友達の解除など)と直列化する(isureal_lock_pair)。
    PERFORM pg_advisory_xact_lock(isureal_pair_lock_key(p_actor, p_author));
  END IF;
  IF NOT (SELECT can_comment FROM isureal_post_access(p_actor, p_post)) THEN
    o_result := 'forbidden'; RETURN NEXT; RETURN;
  END IF;
  INSERT INTO comments (id, post_id, author_id, body, created_at) VALUES (p_id, p_post, p_actor, p_body, clock_timestamp()) RETURNING created_at INTO o_created_at;
  SELECT * INTO u FROM users WHERE id = p_actor AND status = 'active';
  IF NOT FOUND THEN RAISE EXCEPTION 'comment author not found'; END IF;
  RETURN QUERY SELECT 'ok'::text, o_created_at, u.id::text, u.username::text, u.secret_key_hash, u.display_name, u.bio,
    u.profile_image_id::text, u.region, u.account_type, u.status;
END $$;

-- o_result: ok / not_found(404 account_not_found)
CREATE OR REPLACE FUNCTION isureal_put_profile_image(p_user text, p_image_id text, p_data bytea)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  v_old text;
BEGIN
  -- 画像の INSERT は利用者のロックを取る前に済ませる。'not_found' のときはブロックごと巻き戻す。
  BEGIN
    -- 利用者が存在しなければ外部キー違反(23503) → 'not_found'(従来と同じ結果)。
    INSERT INTO image_objects (id, owner_user_id, data, media_type) VALUES (p_image_id, p_user, p_data, 'image/jpeg');
    SELECT profile_image_id INTO v_old FROM users WHERE id = p_user FOR NO KEY UPDATE;
    IF NOT FOUND THEN RAISE SQLSTATE 'IR001'; END IF;
  EXCEPTION
    WHEN SQLSTATE 'IR001' OR foreign_key_violation THEN
      RETURN 'not_found';
  END;
  UPDATE users SET profile_image_id = p_image_id, updated_at = clock_timestamp() WHERE id = p_user;
  -- 初期データの画像(境界以前)は消さない。initialize で users.profile_image_id が初期値に戻るので、
  -- 行が残っていないと初期データと一致しなくなる。外れた画像は image_api の profile_image_id 判定で 403 になる。
  IF v_old IS NOT NULL THEN
    DELETE FROM image_objects WHERE id = v_old AND created_at > (SELECT created_at FROM initial_data_boundary);
  END IF;
  RETURN 'ok';
END $$;

CREATE OR REPLACE FUNCTION isureal_delete_profile_image(p_user text)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  v_old text;
BEGIN
  SELECT profile_image_id INTO v_old FROM users WHERE id = p_user FOR NO KEY UPDATE;
  IF NOT FOUND OR v_old IS NULL THEN RETURN 'not_found'; END IF;
  UPDATE users SET profile_image_id = NULL, updated_at = clock_timestamp() WHERE id = p_user;
  -- 初期データの画像は消さない(isureal_put_profile_image と同じ理由)。
  DELETE FROM image_objects WHERE id = v_old AND created_at > (SELECT created_at FROM initial_data_boundary);
  RETURN 'ok';
END $$;

CREATE OR REPLACE FUNCTION isureal_delete_account(p_user text)
RETURNS text LANGUAGE plpgsql AS $$
BEGIN
  -- 関係の変更(isureal_lock_pair)が共有モードでとる利用者ごとのロックを排他モードでとり、進行中の変更を待つ。
  PERFORM pg_advisory_xact_lock(isureal_user_lock_key(p_user));
  PERFORM 1 FROM users WHERE id = p_user AND status = 'active' FOR NO KEY UPDATE;
  IF NOT FOUND THEN RETURN 'not_found'; END IF;
  UPDATE users SET status = 'deleted', deleted_at = clock_timestamp(), updated_at = clock_timestamp() WHERE id = p_user;
  UPDATE login_sessions SET active = false, invalidated_at = clock_timestamp(), reason = 'account_deletion'
  WHERE user_id = p_user AND active = true;
  RETURN 'ok';
END $$;

-- o_result: ok / not_found(404 post_not_found) / forbidden(403 post_not_editable) / slot_unavailable(409)
CREATE OR REPLACE FUNCTION isureal_delete_post(p_user text, p_post text)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  v_author text;
  v_date date;
  v_deletions int;
BEGIN
  PERFORM 1 FROM users WHERE id = p_user FOR NO KEY UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'user not found'; END IF;
  SELECT p.author_id, w.local_date INTO v_author, v_date
  FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  WHERE p.id = p_post AND p.deleted_at IS NULL;
  IF NOT FOUND THEN RETURN 'not_found'; END IF;
  IF v_author <> p_user THEN RETURN 'forbidden'; END IF;
  SELECT count(*) INTO v_deletions FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  WHERE p.author_id = p_user AND w.local_date = v_date AND p.deleted_at IS NOT NULL;
  IF v_deletions >= 2 THEN RETURN 'slot_unavailable'; END IF;
  UPDATE posts SET deleted_at = clock_timestamp() WHERE id = p_post;
  RETURN 'ok';
END $$;

CREATE OR REPLACE FUNCTION isureal_update_post(p_user text, p_post text, p_caption text, p_visibility text)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  v_author text;
  v_ended timestamptz;
BEGIN
  PERFORM 1 FROM users WHERE id = p_user FOR NO KEY UPDATE;
  IF NOT FOUND THEN RAISE EXCEPTION 'user not found'; END IF;
  SELECT p.author_id, w.ended_at INTO v_author, v_ended
  FROM posts p JOIN posting_windows w ON w.id = p.posting_window_id
  WHERE p.id = p_post AND p.deleted_at IS NULL;
  IF NOT FOUND THEN RETURN 'not_found'; END IF;
  IF v_author <> p_user OR v_ended IS NOT NULL THEN RETURN 'forbidden'; END IF;
  IF p_caption IS NOT NULL OR p_visibility IS NOT NULL THEN
    UPDATE posts SET caption = COALESCE(p_caption, caption), visibility = COALESCE(p_visibility, visibility) WHERE id = p_post;
  END IF;
  RETURN 'ok';
END $$;

-- 通知の id は (投稿期間, 利用者) から決まる値にする。SSE は notifications 表を読まずに、
-- 投稿期間と利用者からこの関数で id を組み立てる (#27)。形式は以前の
-- 「開始時刻 (ミリ秒) の 16 進 12 桁 + gen_random_uuid() の後半」と同じで、後半を
-- md5(期間 id || ':' || 利用者 id) から UUID version 4 / variant 8〜b の形で作る (全体で 36 文字)。
CREATE OR REPLACE FUNCTION isureal_notification_id(p_window_id text, p_user_id text, p_starts_at timestamptz)
RETURNS text LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT substr(ts, 1, 8) || '-' || substr(ts, 9, 4) || '-4' || substr(h, 1, 3) || '-'
         || substr('89ab', (get_byte(decode(h, 'hex'), 2) & 3) + 1, 1) || substr(h, 5, 3) || '-' || substr(h, 8, 12)
  FROM (SELECT lpad(to_hex((extract(epoch FROM p_starts_at) * 1000)::bigint), 12, '0') AS ts,
               md5(p_window_id || ':' || p_user_id) AS h) x
$$;
