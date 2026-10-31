package isureal

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestFollowingFeedOnPostgres(t *testing.T) {
	db := memoryTestDB(t)
	db.MustExec(`
INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline, ended_at) VALUES
 ('asia-now','asia','2026-10-31',now(),now(),NULL),
 ('eu-old','europe','2026-10-31',now(),now(),now()),
 ('eu-now','europe','2026-11-01',now(),now(),NULL);
INSERT INTO login_sessions (id,user_id,token_hash) VALUES ('session','u1','token');
-- A post in another region on the same local date must continue to unlock the feed.
INSERT INTO posts (id,author_id,posting_window_id,image_id,visibility,location_kind,posted_at) VALUES
 ('own','u1','eu-old','img','public','none',now());
INSERT INTO posts (id,author_id,posting_window_id,image_id,visibility,location_kind,posted_at,deleted_at)
 SELECT 'f-'||lpad(i::text,2,'0'), 'u'||i, CASE WHEN i=8 THEN 'eu-old' WHEN i%2=0 THEN 'eu-now' ELSE 'asia-now' END,
 'img', CASE WHEN i IN (3,7) THEN 'friends' WHEN i IN (4,6,9,12) THEN 'friends_of_friends' ELSE 'public' END,
 'none',timestamptz '2026-10-31 12:00Z', CASE WHEN i=10 THEN now() END
 FROM generate_series(1,70) i;
INSERT INTO follows(id,follower_id,followee_id)
 SELECT 'f'||i,'u1','u'||i FROM generate_series(1,70) i WHERE i <> 11;
INSERT INTO friendships(id,user_low_id,user_high_id) VALUES
 ('direct','u1','u3'),('direct-fof','u1','u9'),('mutual','u3','u4'),('reverse-mutual','u12','u3');
INSERT INTO block_relations(id,blocker_id,blocked_id) VALUES ('out','u1','u13'),('in','u14','u1');
`)

	h := &HandlerContext{DB: db, Config: &Config{Region: "asia"}}
	read := func(deferred bool, before *time.Time, id string) []feedRow {
		t.Helper()
		rows, err := selectFeedRows(t.Context(), h, nil, getFeedRequest{
			Feed: "following", BeforePostedAt: before, BeforeID: id,
		}, "u1", "token", deferred)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	checkIDs := func(rows []feedRow, want []string) {
		t.Helper()
		var ids []string
		for _, row := range rows {
			if !row.Found {
				t.Fatal("unexpected empty-page sentinel")
			}
			ids = append(ids, row.ID)
		}
		if !slices.Equal(ids, want) {
			t.Fatalf("got %v, want %v", ids, want)
		}
	}
	var want []string
	for i := 70; i >= 15; i-- {
		want = append(want, fmt.Sprintf("f-%02d", i))
	}
	// u5 is inactive in memoryTestDB. u6/u7 have insufficient visibility, u8's window
	// is closed, u10's post is deleted, u11 is unfollowed, u13/u14 are blocked.
	want = append(want, "f-12", "f-09", "f-04", "f-03", "f-02")
	checkPages := func(want []string) {
		t.Helper()
		for _, deferred := range []bool{false, true} {
			rows := read(deferred, nil, "")
			checkIDs(rows, want[:51])
			cursor := rows[49]
			checkIDs(read(deferred, &cursor.PostedAt, cursor.ID), want[50:])
		}
	}
	checkPages(want)
	// Exercise the small-following plan with exactly 64 follows and a second page.
	db.MustExec(`DELETE FROM follows WHERE id IN ('f66','f67','f68','f69','f70')`)
	checkPages(want[5:])

	// A new block hides u4, and an unfollow immediately removes u3's post without
	// removing their friendship (the mutual path to u12 remains visible).
	db.MustExec(`INSERT INTO block_relations(id,blocker_id,blocked_id) VALUES ('new','u4','u1');
DELETE FROM follows WHERE follower_id='u1' AND followee_id='u3';
UPDATE posts SET deleted_at=now() WHERE id='f-01';`)
	cursorTime := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	checkIDs(read(true, &cursorTime, "f-15"), []string{"f-12", "f-09", "f-02"})
	db.MustExec(`UPDATE posts SET deleted_at=now() WHERE author_id='u1'`)
	if rows := read(true, nil, ""); len(rows) != 1 || rows[0].Found || rows[0].SessRegion == nil || *rows[0].SessRegion != "asia" {
		t.Fatal("ineligible viewer did not get the empty-page sentinel")
	}
	db.MustExec(`UPDATE login_sessions SET active=false WHERE id='session'`)
	if rows := read(true, nil, ""); len(rows) != 0 {
		t.Fatal("revoked session returned feed rows")
	}
}
