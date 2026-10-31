-- isureal_post_access_read (読み取り用の LANGUAGE sql 版) が isureal_post_access (PL/pgSQL、書き込みの関数が使う) と
-- 同じ can_view / can_comment を返すことを確かめる。関係 (自分・友達・友達の友達・他人・ブロックした/された)・
-- 公開範囲 3 種・終わった投稿期間・削除済みの投稿・退会した利用者 (閲覧者/投稿者)・当日投稿していない閲覧者・
-- 別リージョンの閲覧者・存在しない利用者/投稿・NULL (セッションなし) の組み合わせを総当たりする。
-- 公開範囲は、元の混在した状態に加えて、全投稿を public / friends / friends_of_friends にした状態でも比べる。
-- 1 回ずつの呼び出しと、呼び出し側の文と同じ CROSS JOIN LATERAL (列を引数に渡してインライン展開される形) の両方で比べる。
\set ON_ERROR_STOP on
BEGIN;
SET LOCAL statement_timeout = '60s';
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

-- 追加: region を変えた利用者 u-m (america、当日の america の期間は 2099-01-03)。投稿は asia の期間 (2099-01-02) にあるので、
-- 自分の投稿でも「当日未投稿」になる (自分の投稿は見られるがコメントできない)。a とは友達。
DO $$
DECLARE w_us text := gen_random_uuid()::text; w_asia text;
BEGIN
  SELECT id INTO w_asia FROM posting_windows WHERE region = 'asia' AND ended_at IS NULL;
  INSERT INTO users(id, username, secret_key_hash, display_name, region, status) VALUES ('u-m', 'u-m', 's', 'moved', 'america', 'active');
  INSERT INTO posting_windows(id, region, local_date, starts_at, on_time_deadline, ended_at)
  VALUES (w_us, 'america', '2099-01-03', now(), now() + interval '2 min', NULL);
  INSERT INTO friendships(id, user_low_id, user_high_id) VALUES (gen_random_uuid()::text, least('u-a', 'u-m'), greatest('u-a', 'u-m'));
  INSERT INTO posts(id, author_id, posting_window_id, image_id, visibility, location_kind, posted_at)
  VALUES ('p-own-u-m', 'u-m', w_asia, gen_random_uuid()::text, 'friends', 'none', now());
END $$;

DO $$
DECLARE
  actors text[] := ARRAY['u-a', 'u-f', 'u-g', 'u-x', 'u-b', 'u-c', 'u-d', 'u-n', 'u-r', 'u-h', 'u-m', 'u-missing', NULL];
  posts text[];
  vis text;
  act text; pst text;
  o record; nw record;
  n_cases int := 0; n_view int := 0; n_comment int := 0; n_lateral int; n_bad int;
BEGIN
  SELECT array_agg(id ORDER BY id) || ARRAY['p-missing', NULL] INTO posts FROM posts WHERE id LIKE 'p-%';
  FOREACH vis IN ARRAY ARRAY['(元のまま)', 'public', 'friends', 'friends_of_friends'] LOOP
    IF vis <> '(元のまま)' THEN
      UPDATE posts SET visibility = vis WHERE id LIKE 'p-%';
    END IF;
    FOREACH act IN ARRAY actors LOOP
      FOREACH pst IN ARRAY posts LOOP
        SELECT * INTO STRICT o FROM isureal_post_access(act, pst);
        SELECT * INTO STRICT nw FROM isureal_post_access_read(act, pst);
        n_cases := n_cases + 1;
        IF o.can_view IS DISTINCT FROM nw.can_view OR o.can_comment IS DISTINCT FROM nw.can_comment THEN
          RAISE EXCEPTION 'mismatch vis=% viewer=% post=%: old=(%,%) new=(%,%)', vis, act, pst, o.can_view, o.can_comment, nw.can_view, nw.can_comment;
        END IF;
        IF nw.can_view THEN n_view := n_view + 1; END IF;
        IF nw.can_comment THEN n_comment := n_comment + 1; END IF;
      END LOOP;
    END LOOP;
    -- 呼び出し側の文と同じ形 (引数が列、CROSS JOIN LATERAL) で、全組み合わせを 1 文で比べる。
    SELECT count(*), count(*) FILTER (WHERE o2.can_view IS DISTINCT FROM n2.can_view OR o2.can_comment IS DISTINCT FROM n2.can_comment)
      INTO n_lateral, n_bad
    FROM unnest(actors) v(id) CROSS JOIN unnest(posts) p(id)
    CROSS JOIN LATERAL isureal_post_access(v.id, p.id) o2
    CROSS JOIN LATERAL isureal_post_access_read(v.id, p.id) n2;
    IF n_bad <> 0 OR n_lateral <> cardinality(actors) * cardinality(posts) THEN
      RAISE EXCEPTION 'lateral mismatch vis=%: rows=% bad=%', vis, n_lateral, n_bad;
    END IF;
  END LOOP;
  RAISE WARNING 'post_access_read: % cases identical (can_view=true %, can_comment=true %), lateral rows also identical', n_cases, n_view, n_comment;
END $$;
ROLLBACK;
