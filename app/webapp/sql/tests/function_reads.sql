\set ON_ERROR_STOP on
BEGIN;
DO $$
DECLARE
  a text := 'function-reads-a';
  b text := 'function-reads-b';
  mid text := 'function-reads-mid';
  w text := 'function-reads-window';
  p text := 'function-reads-post';
  r record;
  mask int;
  vis text;
  owner text;
  checked int := 0;
BEGIN
  SELECT * INTO r FROM isureal_register(a, a, repeat('a',64), 'Alice', 'asia');
  ASSERT r.o_result = 'ok' AND r.o_id = a AND r.o_bio = '' AND r.o_status = 'active';
  ASSERT (SELECT o_result = 'username_taken' FROM isureal_register('duplicate', a, 'key', 'A', 'asia'));
  INSERT INTO users(id,username,secret_key_hash,display_name,region)
    VALUES(b,b,repeat('b',64),'Bob','asia'),(mid,mid,repeat('c',64),'Middle','asia');
  INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline)
    VALUES(w,'asia',CURRENT_DATE,clock_timestamp()-interval '1 hour',clock_timestamp()+interval '1 hour');
  SELECT * INTO r FROM isureal_create_post(a,'function-reads-image',p,'\x1234','hello','public','exact',12.34,56.78,NULL,NULL);
  ASSERT r.o_result = 'ok' AND r.o_id = a AND r.o_window_id = w;
  ASSERT (SELECT (posted_at,latitude::float8,longitude::float8,locality) IS NOT DISTINCT FROM
                 (r.o_posted_at,r.o_latitude,r.o_longitude,r.o_locality) FROM posts WHERE id=p);
  ASSERT (SELECT o_result = 'slot_unavailable' FROM isureal_create_post(a,'unused-image','unused-post','\x12','','public','none',NULL,NULL,NULL,NULL));
  ASSERT NOT EXISTS(SELECT 1 FROM image_objects WHERE id='unused-image');
  ASSERT isureal_update_post(a,p,'changed','friends')='ok';
  ASSERT (SELECT caption='changed' AND visibility='friends' FROM posts WHERE id=p);
  ASSERT isureal_update_post(a,p,NULL,'public')='ok';
  ASSERT (SELECT caption='changed' AND visibility='public' FROM posts WHERE id=p);

  SELECT * INTO r FROM isureal_relationship('friend_request',a,b,'function-reads-request');
  ASSERT r.o_result='ok' AND r.o_status='pending';
  ASSERT (SELECT (id::text,status,updated_at)=(r.o_id,r.o_status,r.o_updated_at) FROM friend_requests WHERE id=r.o_id);
  SELECT * INTO r FROM isureal_relationship('friend_request',b,a,'unused-request');
  ASSERT r.o_result='ok' AND r.o_status='accepted' AND r.o_id='function-reads-request';
  ASSERT (SELECT updated_at=r.o_updated_at FROM friend_requests WHERE id=r.o_id);
  SELECT * INTO r FROM isureal_relationship('friend_request',a,mid,'function-reads-request2');
  SELECT * INTO r FROM isureal_transition_friend_request_authorized(mid,r.o_id,'accepted');
  ASSERT r.o_result='ok' AND r.o_status='accepted';
  ASSERT (SELECT updated_at=r.o_updated_at FROM friend_requests WHERE id=r.o_id);
  INSERT INTO posts(id,author_id,posting_window_id,image_id,visibility,location_kind,posted_at)
    VALUES('function-reads-viewer-post',b,w,'unused','public','none',now());
  SELECT * INTO r FROM isureal_create_comment(b,p,'function-reads-comment','body');
  ASSERT r.o_result='ok' AND r.o_id=b;
  ASSERT (SELECT created_at=r.o_created_at FROM comments WHERE id='function-reads-comment');
  SELECT * INTO r FROM isureal_replace_realmoji(b,p,'function-reads-realmoji','function-reads-realmoji-image','\x12','like',NULL);
  ASSERT r.o_result='ok' AND r.o_id=b;
  ASSERT (SELECT created_at=r.o_created_at FROM realmojis WHERE id='function-reads-realmoji');
  SELECT * INTO r FROM isureal_replace_realmoji(b,p,'function-reads-realmoji2','function-reads-realmoji-image2','\x12','smile',NULL);
  ASSERT r.o_result='ok';
  ASSERT (SELECT replaced_at IS NOT NULL FROM realmojis WHERE id='function-reads-realmoji');

  -- Compare to the pre-change function installed by the runner. Every combination
  -- includes ownership, visibility, window closure, deleted users/posts, posting,
  -- friendship, a mutual friend, and both block directions.
  FOREACH vis IN ARRAY ARRAY['public','friends','friends_of_friends'] LOOP
    FOR mask IN 0..511 LOOP
      DELETE FROM friendships;
      DELETE FROM block_relations;
      UPDATE users SET status=CASE WHEN id=a AND (mask&1)<>0 OR id=b AND (mask&2)<>0 THEN 'deleted' ELSE 'active' END;
      UPDATE posting_windows SET ended_at=CASE WHEN (mask&4)<>0 THEN now() ELSE NULL END WHERE id=w;
      UPDATE posts SET deleted_at=CASE WHEN (mask&8)<>0 THEN now() ELSE NULL END,visibility=vis WHERE id=p;
      UPDATE posts SET deleted_at=CASE WHEN (mask&16)<>0 THEN now() ELSE NULL END WHERE id='function-reads-viewer-post';
      IF (mask&32)<>0 THEN INSERT INTO friendships(id,user_low_id,user_high_id) VALUES(gen_random_uuid()::text,least(a,b),greatest(a,b)); END IF;
      IF (mask&64)<>0 THEN
        INSERT INTO friendships(id,user_low_id,user_high_id) VALUES(gen_random_uuid()::text,least(a,mid),greatest(a,mid)),(gen_random_uuid()::text,least(b,mid),greatest(b,mid));
      END IF;
      IF (mask&128)<>0 THEN INSERT INTO block_relations(id,blocker_id,blocked_id) VALUES(gen_random_uuid()::text,a,b); END IF;
      IF (mask&256)<>0 THEN INSERT INTO block_relations(id,blocker_id,blocked_id) VALUES(gen_random_uuid()::text,b,a); END IF;
      FOREACH owner IN ARRAY ARRAY[a,b] LOOP
        ASSERT (SELECT ROW(n.can_view,n.can_comment) IS NOT DISTINCT FROM ROW(o.can_view,o.can_comment)
                FROM isureal_post_access(owner,p) n, isureal_post_access_baseline(owner,p) o),
               format('access differs: visibility=%s mask=%s viewer=%s',vis,mask,owner);
        checked := checked+1;
      END LOOP;
    END LOOP;
  END LOOP;
  RAISE NOTICE 'PASS: mutation results and % access comparisons',checked;
END $$;
ROLLBACK;
