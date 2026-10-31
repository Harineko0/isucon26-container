package isureal

import (
	"fmt"
	"slices"
	"testing"
	"time"
)

func TestPublicFeedOnPostgres(t *testing.T) {
	db := memoryTestDB(t)
	db.MustExec(`
INSERT INTO posting_windows (id,region,local_date,starts_at,on_time_deadline,ended_at) VALUES
 ('asia-now','asia','2026-10-31',now(),now(),NULL),
 ('eu-old','europe','2026-10-31',now(),now(),now()),
 ('eu-now','europe','2026-11-01',now(),now(),NULL);
INSERT INTO login_sessions(id,user_id,token_hash) VALUES ('s','u1','token');
-- The viewer posted in Europe before moving to Asia on the same local date.
INSERT INTO posts(id,author_id,posting_window_id,image_id,visibility,location_kind,posted_at)
 VALUES ('own','u1','eu-old','own-img','public','none',now());
INSERT INTO posts(id,author_id,posting_window_id,image_id,visibility,location_kind,posted_at,deleted_at)
 SELECT 'pub-'||lpad(i::text,2,'0'),'u'||i,
  CASE WHEN i=70 THEN 'eu-old' WHEN i=1 OR i%2=0 THEN 'eu-now' ELSE 'asia-now' END,
  'img-'||i, CASE WHEN i=69 THEN 'friends' WHEN i=68 THEN 'friends_of_friends' ELSE 'public' END,
  'none', timestamptz '2026-10-31 12:00Z', CASE WHEN i=67 THEN now() END
 FROM generate_series(1,70) i;
INSERT INTO block_relations(id,blocker_id,blocked_id) VALUES ('out','u1','u66'),('in','u65','u1');
UPDATE users SET status='deleted' WHERE id='u64';`)

	var want []string
	for i := 63; i >= 2; i-- {
		if i != 5 { // memoryTestDB seeds u5 as deleted
			want = append(want, fmt.Sprintf("pub-%02d", i))
		}
	}
	h := &HandlerContext{DB: db, Config: &Config{Region: "asia"}}
	read := func(deferred bool, before *time.Time, id string) []feedRow {
		t.Helper()
		rows, err := selectFeedRows(t.Context(), h, nil, getFeedRequest{Feed: "public", BeforePostedAt: before, BeforeID: id}, "u1", "token", deferred)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	check := func(rows []feedRow, ids []string) {
		t.Helper()
		var got []string
		for _, r := range rows {
			if !r.Found || r.Author.ID != r.AuthorID {
				t.Fatalf("unexpected sentinel or author on %q", r.ID)
			}
			got = append(got, r.ID)
		}
		if !slices.Equal(got, ids) {
			t.Fatalf("got %v, want %v", got, ids)
		}
	}
	for _, deferred := range []bool{false, true} {
		first := read(deferred, nil, "")
		check(first, want[:51])
		cursor := first[49]
		check(read(deferred, &cursor.PostedAt, cursor.ID), want[50:])
	}
	db.MustExec(`UPDATE posts SET visibility='friends' WHERE id='pub-63';
UPDATE users SET status='deleted' WHERE id='u62'`)
	check(read(true, nil, ""), want[2:53])
	db.MustExec(`UPDATE posts SET deleted_at=now() WHERE author_id='u1'`)
	if rows := read(true, nil, ""); len(rows) != 1 || rows[0].Found || rows[0].SessRegion == nil || *rows[0].SessRegion != "asia" {
		t.Fatal("viewer without a post must receive the empty-page sentinel")
	}
	db.MustExec(`UPDATE login_sessions SET active=false WHERE id='s'`)
	if rows := read(true, nil, ""); len(rows) != 0 {
		t.Fatal("revoked session returned a row")
	}
}
