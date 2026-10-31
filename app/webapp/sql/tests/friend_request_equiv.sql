-- 友達申請の送信 (isureal_relationship) と承認・拒否・取り下げ (isureal_transition_friend_request_authorized) を、
-- origin/main の関数 (*_base、friend_request_equiv.sh が入れる) と今の関数で同じ状態から 1 手ずつ実行し、
-- 結果 (行数・o_result・o_id・o_status) と実行後の関係の表がすべて一致することを確かめる。
-- 旧版は副トランザクションで実行して巻き戻し、新版の結果を残して次の手に進む (状態は新版で進む)。
CREATE OR REPLACE FUNCTION fr_state() RETURNS text LANGUAGE sql AS $$
SELECT concat_ws(' | ',
  (SELECT string_agg(concat_ws(',', id, requester_id, recipient_id, status), ';' ORDER BY id) FROM friend_requests),
  (SELECT string_agg(concat_ws(',', user_low_id, user_high_id, source_request_id), ';' ORDER BY user_low_id, user_high_id) FROM friendships),
  (SELECT string_agg(concat_ws(',', id, follower_id, followee_id), ';' ORDER BY id) FROM follows),
  (SELECT string_agg(concat_ws(',', id, blocker_id, blocked_id), ';' ORDER BY id) FROM block_relations))
$$;

-- 1 手を実行し、結果と実行後の状態を 1 つの文字列にする。o_updated_at は行の updated_at と一致するかだけを見る。
CREATE OR REPLACE FUNCTION fr_step(p_base boolean, p_kind text, p_actor text, p_target text, p_arg text)
RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  v_res text;
BEGIN
  IF p_kind = 'transition' THEN
    IF p_base THEN
      SELECT count(*) || ':' || coalesce(string_agg(concat_ws(',', o_result, o_id, o_status,
               o_id IS NULL OR o_updated_at = (SELECT updated_at FROM friend_requests WHERE id = o_id)), ';'), '')
        INTO v_res FROM isureal_transition_friend_request_authorized_base(p_actor, p_target, p_arg);
    ELSE
      SELECT count(*) || ':' || coalesce(string_agg(concat_ws(',', o_result, o_id, o_status,
               o_id IS NULL OR o_updated_at = (SELECT updated_at FROM friend_requests WHERE id = o_id)), ';'), '')
        INTO v_res FROM isureal_transition_friend_request_authorized(p_actor, p_target, p_arg);
    END IF;
  ELSE
    IF p_base THEN
      SELECT count(*) || ':' || coalesce(string_agg(concat_ws(',', o_result, o_id, o_status,
               o_id IS NULL OR o_updated_at = (SELECT updated_at FROM friend_requests WHERE id = o_id)), ';'), '')
        INTO v_res FROM isureal_relationship_base(p_kind, p_actor, p_target, p_arg);
    ELSE
      SELECT count(*) || ':' || coalesce(string_agg(concat_ws(',', o_result, o_id, o_status,
               o_id IS NULL OR o_updated_at = (SELECT updated_at FROM friend_requests WHERE id = o_id)), ';'), '')
        INTO v_res FROM isureal_relationship(p_kind, p_actor, p_target, p_arg);
    END IF;
  END IF;
  RETURN v_res || ' || ' || fr_state();
EXCEPTION WHEN OTHERS THEN
  RETURN 'error ' || SQLSTATE || ' || ' || fr_state();
END $$;

DO $$
DECLARE
  users text[] := ARRAY['eq01','eq02','eq03','eq04','eq05','eq06','eq07','eq08','eq09'];
  kinds text[] := ARRAY['friend_request','friend_request','friend_request','friend_request','transition','transition','transition','transition','transition',
                        'block','unblock','unfriend','follow','unfollow'];
  statuses text[] := ARRAY['accepted','accepted','accepted','rejected','cancelled','bogus'];
  i int;
  k text; a text; b text; arg text;
  r_base text; r_new text;
  n_steps int := 6000;
  counts jsonb := '{}';
  key text;
BEGIN
  -- 使い捨ての DB なので、種を変えて流し直すときは前の回の行を消す
  TRUNCATE friend_requests, friendships, follows, block_relations;
  DELETE FROM users WHERE id LIKE 'eq%';
  PERFORM setseed(0.42);
  INSERT INTO users (id, username, secret_key_hash, display_name, region, account_type)
  SELECT u, u, 'h', u, 'asia', CASE WHEN u IN ('eq07','eq08') THEN 'certified_public' ELSE 'regular' END FROM unnest(users) u;
  -- 初期データにありうる形: 両方向の pending、友達なのに残っている pending、ブロック中の pending
  INSERT INTO friend_requests VALUES
    ('init-1','eq01','eq02','pending',now(),now()), ('init-2','eq02','eq01','pending',now(),now()),
    ('init-3','eq03','eq04','pending',now(),now()), ('init-4','eq05','eq06','pending',now(),now()),
    ('init-5','eq07','eq07','pending',now(),now());
  INSERT INTO friendships (id, user_low_id, user_high_id) VALUES ('init-f','eq03','eq04');
  INSERT INTO block_relations (id, blocker_id, blocked_id) VALUES ('init-b','eq06','eq05');
  FOR i IN 1..n_steps LOOP
    k := kinds[1 + floor(random() * array_length(kinds, 1))::int];
    a := users[1 + floor(random() * 9)::int];
    b := users[1 + floor(random() * 9)::int];
    IF random() < 0.03 THEN  -- 退会 (status) を時々切り替え、not_found の経路も通す
      -- 退会中は高々 1 人 (いれば戻し、いなければ a を退会にする)
      IF EXISTS(SELECT 1 FROM users WHERE id LIKE 'eq%' AND status = 'deleted') THEN
        UPDATE users SET status = 'active' WHERE id LIKE 'eq%' AND status = 'deleted';
      ELSE
        UPDATE users SET status = 'deleted' WHERE id = a;
      END IF;
      CONTINUE;
    END IF;
    IF random() < 0.04 AND a <> b THEN
      -- 関数を通らない pending (初期データにありうる、ブロック中・友達なのに・同じ 2 人に 2 件) を時々足す
      INSERT INTO friend_requests VALUES ('stray-' || i, a, b, 'pending', now(), now());
      CONTINUE;
    END IF;
    IF k = 'transition' THEN
      arg := statuses[1 + floor(random() * array_length(statuses, 1))::int];
      IF random() < 0.08 THEN
        b := 'missing-' || i;
      ELSE
        -- 7 割は pending、残りは処理済み (承認・拒否・取り下げ済みへの操作は 403) から選ぶ
        SELECT id INTO b FROM friend_requests WHERE (status = 'pending') = (random() < 0.7) ORDER BY random() LIMIT 1;
        IF b IS NULL THEN SELECT id INTO b FROM friend_requests ORDER BY random() LIMIT 1; END IF;
        IF random() < 0.5 THEN
          SELECT CASE WHEN arg = 'cancelled' THEN requester_id ELSE recipient_id END INTO a FROM friend_requests WHERE id = b;
        END IF;
      END IF;
    ELSE
      arg := 'new-' || i;
    END IF;
    BEGIN
      r_base := fr_step(true, k, a, b, arg);
      RAISE SQLSTATE 'XX999';
    EXCEPTION WHEN SQLSTATE 'XX999' THEN NULL;
    END;
    r_new := fr_step(false, k, a, b, arg);
    IF r_base IS DISTINCT FROM r_new THEN
      RAISE EXCEPTION E'mismatch at step % (% % % %)\nbase=%\nnew=%', i, k, a, b, arg, r_base, r_new;
    END IF;
    key := k || ' ' || split_part(split_part(r_new, ' || ', 1), ',', 1);
    counts := jsonb_set(counts, ARRAY[key], to_jsonb(coalesce((counts->>key)::int, 0) + 1));
  END LOOP;
  RAISE NOTICE 'friend_request_equiv: % steps identical: %', n_steps, counts;
END $$;
