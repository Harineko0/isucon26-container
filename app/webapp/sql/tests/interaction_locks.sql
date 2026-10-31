-- isureal_replace_realmoji / isureal_create_comment の新旧 (*_old) が、同じ状態から同じ結果・同じ書き込みになることを確かめる。
-- 関係 (友達・友達の友達・ブロック)・公開範囲・終わった投稿期間・削除済みの投稿・退会した利用者・投稿していない閲覧者・
-- 別リージョンの閲覧者・存在しない利用者/投稿の組み合わせを総当たりする。各呼び出しはサブトランザクションで巻き戻す。
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL statement_timeout = '60s';
CREATE TEMP TABLE fx(k text PRIMARY KEY, v text) ON COMMIT DROP;
DO $$
DECLARE
  a text := 'u-a'; f text := 'u-f'; g text := 'u-g'; x text := 'u-x'; b text := 'u-b'; c text := 'u-c';
  d text := 'u-d'; n text := 'u-n'; r text := 'u-r'; h text := 'u-h';
  w_asia text := gen_random_uuid()::text; w_old text := gen_random_uuid()::text; w_eu text := gen_random_uuid()::text;
  u text;
BEGIN
  INSERT INTO users(id, username, secret_key_hash, display_name, region, status) VALUES
    (a, a, 's', 'author', 'asia', 'active'), (f, f, 's', 'friend', 'asia', 'active'),
    (g, g, 's', 'fof', 'asia', 'active'), (x, x, 's', 'stranger', 'asia', 'active'),
    (b, b, 's', 'blocked', 'asia', 'active'), (c, c, 's', 'blocker', 'asia', 'active'),
    (d, d, 's', 'deleted', 'asia', 'deleted'), (n, n, 's', 'notposted', 'asia', 'active'),
    (r, r, 's', 'europe', 'europe', 'active'), (h, h, 's', 'friend-notposted', 'asia', 'active');
  INSERT INTO posting_windows(id, region, local_date, starts_at, on_time_deadline, ended_at) VALUES
    (w_asia, 'asia', '2099-01-02', now(), now() + interval '2 min', NULL),
    (w_old, 'asia', '2099-01-01', now() - interval '1 day', now() - interval '1 day', now() - interval '1 hour'),
    (w_eu, 'europe', '2099-01-02', now(), now() + interval '2 min', NULL);
  INSERT INTO friendships(id, user_low_id, user_high_id) VALUES
    (gen_random_uuid()::text, least(a, f), greatest(a, f)),
    (gen_random_uuid()::text, least(f, g), greatest(f, g)),
    (gen_random_uuid()::text, least(a, h), greatest(a, h)),
    (gen_random_uuid()::text, least(a, b), greatest(a, b));
  INSERT INTO block_relations(id, blocker_id, blocked_id) VALUES
    (gen_random_uuid()::text, a, b), (gen_random_uuid()::text, c, a);
  -- 閲覧者は (n・h 以外) 今の期間に投稿済み。a の投稿は公開範囲ごと + 終わった期間 + 削除済み。d の投稿 (退会者)。
  FOREACH u IN ARRAY ARRAY[a, f, g, x, b, c, d] LOOP
    INSERT INTO posts(id, author_id, posting_window_id, image_id, visibility, location_kind, posted_at)
    VALUES ('p-own-' || u, u, w_asia, gen_random_uuid()::text, 'friends', 'none', now());
  END LOOP;
  INSERT INTO posts(id, author_id, posting_window_id, image_id, visibility, location_kind, posted_at)
  VALUES ('p-own-' || r, r, w_eu, gen_random_uuid()::text, 'public', 'none', now());
  UPDATE posts SET visibility = 'public' WHERE id = 'p-own-u-d';
  INSERT INTO posts(id, author_id, posting_window_id, image_id, visibility, location_kind, posted_at, deleted_at) VALUES
    ('p-pub', a, w_asia, gen_random_uuid()::text, 'public', 'none', now(), now()),
    ('p-old', a, w_old, gen_random_uuid()::text, 'public', 'none', now() - interval '1 day', NULL),
    ('p-gone', a, w_old, gen_random_uuid()::text, 'public', 'none', now() - interval '1 day', now());
  -- 今の期間の a の投稿は 1 件だけ (p-own-u-a)。公開範囲を変えた版を別の投稿者にも作る。
  INSERT INTO posts(id, author_id, posting_window_id, image_id, visibility, location_kind, posted_at)
  SELECT 'p-' || t.vis || '-' || o.uid, o.uid, w_asia, gen_random_uuid()::text, t.vis, 'none', now()
  FROM unnest(ARRAY['public', 'friends', 'friends_of_friends']) t(vis), unnest(ARRAY[f, x]) o(uid);
  UPDATE posts SET posting_window_id = w_old WHERE id LIKE 'p-%-u-x' AND id <> 'p-own-u-x';
END $$;

-- 1 回の呼び出しを巻き戻して、結果と書き込み (RealMoji / コメント / 画像の行) を 1 つの文字列にする。
CREATE OR REPLACE FUNCTION pg_temp.try_call(p_fn text, p_actor text, p_post text) RETURNS text LANGUAGE plpgsql AS $$
DECLARE
  res text;
BEGIN
  BEGIN
    IF p_fn LIKE 'isureal_replace_realmoji%' THEN
      EXECUTE format($q$SELECT string_agg(concat_ws('|', o_result, o_created_at IS NOT NULL, o_id, o_username, o_secret_key_hash,
          o_display_name, o_bio, o_profile_image_id, o_region, o_account_type, o_status), ';')
        FROM %I($1, $2, 'rm-new', 'img-new', '\x00'::bytea, 'smile', NULL)$q$, p_fn) INTO res USING p_actor, p_post;
    ELSE
      EXECUTE format($q$SELECT string_agg(concat_ws('|', o_result, o_created_at IS NOT NULL, o_id, o_username, o_secret_key_hash,
          o_display_name, o_bio, o_profile_image_id, o_region, o_account_type, o_status), ';')
        FROM %I($1, $2, 'cm-new', 'hello')$q$, p_fn) INTO res USING p_actor, p_post;
    END IF;
    res := res || ' rm=' || (SELECT count(*) FROM realmojis WHERE id = 'rm-new')
               || ' rm_live=' || (SELECT count(*) FROM realmojis WHERE reactor_id = p_actor AND post_id = p_post AND replaced_at IS NULL)
               || ' cm=' || (SELECT count(*) FROM comments WHERE id = 'cm-new')
               || ' img=' || (SELECT count(*) FROM image_objects WHERE id = 'img-new');
    RAISE SQLSTATE 'IR999' USING MESSAGE = res;
  EXCEPTION
    WHEN SQLSTATE 'IR999' THEN RETURN SQLERRM;
    WHEN OTHERS THEN RETURN 'ERR ' || SQLSTATE || ' ' || SQLERRM;
  END;
END $$;

DO $$
DECLARE
  actors text[] := ARRAY['u-a', 'u-f', 'u-g', 'u-x', 'u-b', 'u-c', 'u-d', 'u-n', 'u-r', 'u-h', 'u-missing'];
  posts text[];
  act text; pst text; fn text; o text; nw text;
  n_cases int := 0; n_ok int := 0; n_forbidden int := 0; n_other int := 0;
BEGIN
  SELECT array_agg(id ORDER BY id) || ARRAY['p-missing'] INTO posts FROM posts WHERE id LIKE 'p-%';
  -- 既存の RealMoji (置き換えの対象) を 1 件入れておく。
  INSERT INTO realmojis(id, post_id, reactor_id, image_id, reaction_type, created_at)
  VALUES ('rm-existing', 'p-own-u-a', 'u-f', 'img-existing', 'smile', now());
  FOREACH fn IN ARRAY ARRAY['isureal_replace_realmoji', 'isureal_create_comment'] LOOP
    FOREACH act IN ARRAY actors LOOP
      FOREACH pst IN ARRAY posts LOOP
        o := pg_temp.try_call(fn || '_old', act, pst);
        nw := pg_temp.try_call(fn, act, pst);
        n_cases := n_cases + 1;
        IF o IS DISTINCT FROM nw THEN
          RAISE EXCEPTION 'mismatch % actor=% post=%: old=[%] new=[%]', fn, act, pst, o, nw;
        END IF;
        IF nw LIKE 'ok|%' THEN n_ok := n_ok + 1; ELSIF nw LIKE 'forbidden%' THEN n_forbidden := n_forbidden + 1; ELSE n_other := n_other + 1; END IF;
      END LOOP;
    END LOOP;
  END LOOP;
  RAISE WARNING 'interaction_locks: % cases identical (ok=%, forbidden=%, other=%)', n_cases, n_ok, n_forbidden, n_other;
END $$;
ROLLBACK;
