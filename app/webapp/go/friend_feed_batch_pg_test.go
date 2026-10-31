package isureal

import (
	"reflect"
	"testing"
)

// memoryTestDB provides an isolated PostgreSQL schema with 70 users and
// closed historical windows. No shared application tables are modified.
func TestFriendFeedBatchOnPostgres(t *testing.T) {
	db := memoryTestDB(t)
	db.MustExec(`
INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline,ended_at) VALUES
 ('asia-now','asia','2026-10-31',now(),now()+interval '1 day',NULL),
 ('europe-old','europe','2026-10-31',now()-interval '1 day',now(),now()),
 ('europe-now','europe','2026-11-01',now(),now()+interval '1 day',NULL);
UPDATE users SET region='europe' WHERE id='u4';
UPDATE users SET status='deleted' WHERE id='u68';
INSERT INTO login_sessions(id,user_id,token_hash,active)
 SELECT 's'||i,'u'||i,'t'||i,i<>6 FROM generate_series(1,6) i;
-- u1 posted in another region's now-closed window, but on the current local date in asia.
INSERT INTO posts(id,author_id,posting_window_id,image_id,visibility,location_kind,posted_at) VALUES
 ('own-u1','u1','europe-old','img1','public','none',now()),
 ('own-u2','u2','asia-now','img2','public','none',timestamptz '2026-10-31 15:00Z');
INSERT INTO posts(id,author_id,posting_window_id,image_id,caption,visibility,location_kind,latitude,longitude,posted_at,deleted_at)
 SELECT 'batch-'||lpad(i::text,2,'0'),'u'||i,CASE WHEN i%2=0 THEN 'asia-now' ELSE 'europe-now' END,
    'img'||i,'caption '||i,CASE WHEN i=66 THEN 'private' ELSE (ARRAY['friends','public','friends_of_friends'])[1+i%3] END,
    'exact',35.0,139.0,timestamptz '2026-10-31 12:00Z'+(i/4)*interval '1 second',CASE WHEN i=67 THEN now() END
 FROM generate_series(7,70) i;
INSERT INTO friendships(id,user_low_id,user_high_id)
 SELECT 'main-'||i,'u1','u'||i FROM generate_series(7,70) i;
INSERT INTO friendships(id,user_low_id,user_high_id) VALUES
 ('f2-10','u10','u2'),('f2-70','u2','u70'),('f3-10','u10','u3'),('f1-2','u1','u2');
INSERT INTO block_relations(id,blocker_id,blocked_id) VALUES ('b69','u1','u69'),('b70','u70','u1');
`)

	conn, err := db.Connx(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	calls := []friendFeedRequest{
		{userID: "u1", tokenHash: "t1"}, {userID: "u2", tokenHash: "t2"},
		{userID: "u1", tokenHash: "t2"}, {userID: "u4", tokenHash: "t4"},
		{userID: "u3", tokenHash: "t3"}, {userID: "u1", tokenHash: "t1"},
		{userID: "u6", tokenHash: "t6"}, {userID: "u5", tokenHash: "t5"},
	}
	compare := func() [][]feedRow {
		t.Helper()
		batch, err := selectFriendFeedBatch(t.Context(), conn, "asia", calls)
		if err != nil {
			t.Fatal(err)
		}
		if len(batch) != len(calls) {
			t.Fatalf("got %d result sets for %d requests", len(batch), len(calls))
		}
		for i, call := range calls {
			var want []feedRow
			if err := conn.SelectContext(t.Context(), &want, feedDeferredQueries["friends"], call.userID, nil, "", "asia", call.tokenHash); err != nil {
				t.Fatal(err)
			}
			if len(batch[i]) != len(want) {
				t.Fatalf("request %d (%s): got %d rows, single query returned %d", i, call.userID, len(batch[i]), len(want))
			}
			for j := range want {
				if !reflect.DeepEqual(batch[i][j], want[j]) {
					t.Fatalf("request %d row %d: batch %+v, single %+v", i, j, batch[i][j], want[j])
				}
			}
		}
		return batch
	}

	rows := compare()
	if len(rows[0]) != 51 || rows[0][1].ID != "batch-65" || len(rows[1]) != 2 || rows[1][0].ID != "batch-70" {
		t.Fatal("fixture did not exercise the 51-row limit, tie order and per-viewer exclusions")
	}
	if len(rows[2]) != 0 || len(rows[6]) != 0 || len(rows[7]) != 0 {
		t.Fatal("wrong-user, revoked or deleted-user session was accepted")
	}
	for i, region := range map[int]string{3: "europe", 4: "asia"} {
		if len(rows[i]) != 1 || rows[i][0].Found || rows[i][0].SessRegion == nil || *rows[i][0].SessRegion != region {
			t.Fatalf("request %d has wrong empty-page sentinel", i)
		}
	}

	// The write commits on a different connection. A later batch must reflect
	// the logout, the new block and the changed post in the same response.
	db.MustExec(`
UPDATE login_sessions SET active=false WHERE user_id='u1';
INSERT INTO block_relations(id,blocker_id,blocked_id) VALUES ('new-block','u2','u70');
UPDATE posts SET caption='edited' WHERE id='batch-10';`)
	rows = compare()
	if len(rows[0]) != 0 || len(rows[5]) != 0 || len(rows[1]) != 1 || rows[1][0].Caption != "edited" {
		t.Fatal("batch returned stale data after a committed change")
	}
}
