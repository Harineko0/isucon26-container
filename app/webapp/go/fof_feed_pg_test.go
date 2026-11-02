package isureal

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

func installTestFriendGraph(t *testing.T, db *sqlx.DB) {
	t.Helper()
	graphDDL, err := os.ReadFile("../sql/009-friend-graph.sql")
	if err != nil {
		t.Fatal(err)
	}
	db.MustExec(string(graphDDL))
}

func TestFriendsOfFriendsFeedOnPostgres(t *testing.T) {
	db := memoryTestDB(t)
	installTestFriendGraph(t, db)
	db.MustExec(`
INSERT INTO login_sessions(id,user_id,token_hash) VALUES ('session','u1','token');
INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline,ended_at) VALUES
 ('asia-now','asia','2026-10-31','2026-10-31 10:00Z','2026-10-31 12:00Z',NULL),
 ('eu-old','europe','2026-10-31',now(),now(),now()),
 ('eu-now','europe','2026-11-01','2026-10-31 10:00Z','2026-10-31 12:00Z',NULL);
INSERT INTO friendships(id,user_low_id,user_high_id) VALUES
 ('direct-2','u1','u2'),('direct-3','u1','u3'),('second-path','u3','u6'),('shared-edge','u6','u7'),('self','u6','u6');
INSERT INTO friendships(id,user_low_id,user_high_id)
 SELECT 'bridge-'||i, least('u2','u'||i), greatest('u2','u'||i) FROM generate_series(4,70) i WHERE i<>12;
INSERT INTO block_relations(id,blocker_id,blocked_id) VALUES ('out','u1','u8'),('in','u9','u1');
INSERT INTO posts(id,author_id,posting_window_id,image_id,caption,visibility,location_kind,latitude,longitude,posted_at,deleted_at)
 SELECT 'fof-'||lpad(i::text,2,'0'), 'u'||i,
 CASE WHEN i IN (1,11) THEN 'eu-old' WHEN i%2=0 THEN 'eu-now' ELSE 'asia-now' END,
 'img-'||i,'caption '||i, CASE WHEN i=4 THEN 'friends' WHEN i%2=0 THEN 'friends_of_friends' ELSE 'public' END,
 'exact',35.5,139.5, timestamptz '2026-10-31 11:00Z'+CASE WHEN i<=13 THEN interval '1 minute' ELSE interval '0' END,
 CASE WHEN i=10 THEN now() END FROM generate_series(1,70) i;
INSERT INTO comments(id,post_id,author_id,body,created_at,deleted_at) VALUES
 ('c2','fof-06','u2','',now(),NULL), ('c3','fof-06','u3','',now(),NULL),
 ('non-friend','fof-06','u4','',now(),NULL), ('deleted','fof-07','u3','',now(),now());
INSERT INTO realmojis(id,post_id,reactor_id,image_id,reaction_type,created_at,replaced_at) VALUES
 ('r2','fof-06','u2','r2-img','smile',now(),NULL), ('old-r2','fof-07','u2','old-img','smile',now(),now());
`)
	h := &HandlerContext{DB: db, Config: &Config{Region: "asia"}}
	read := func(deferred bool, cursor *feedRow) []feedRow {
		t.Helper()
		input := getFeedRequest{Feed: "friends-of-friends"}
		if cursor != nil {
			input.BeforeScore, input.BeforePostedAt, input.BeforeID = &cursor.Score, &cursor.PostedAt, cursor.ID
		}
		rows, err := selectFeedRows(t.Context(), h, nil, input, "u1", "token", deferred)
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}

	// u6 has two mutual friends (6 points) and two engaging friends (20). Its
	// neighbors u7 and u6 itself contribute 1 + 2 more. The self-edge is counted once.
	type entry struct {
		number int
		score  int64
	}
	want := []entry{{6, 29}, {7, 5}, {13, 3}}
	for n := 70; n >= 14; n-- {
		want = append(want, entry{n, 3})
	}
	deadline := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	checkPage := func(rows []feedRow, expected []entry) {
		t.Helper()
		if len(rows) != len(expected) {
			t.Fatalf("got %d rows, want %d", len(rows), len(expected))
		}
		for i, e := range expected {
			row := rows[i]
			if !row.Found || row.ID != fmt.Sprintf("fof-%02d", e.number) || row.Score != e.score {
				t.Fatalf("row %d: got %q score %d, want post %d score %d", i, row.ID, row.Score, e.number, e.score)
			}
			if row.ImageID != fmt.Sprintf("img-%d", e.number) || row.Caption != fmt.Sprintf("caption %d", e.number) ||
				row.Author.ID != row.AuthorID || row.Author.Bio != fmt.Sprintf("bio %d", e.number) ||
				row.Author.SecretKeyHash != fmt.Sprintf("%064x", e.number) || row.Kind != "exact" ||
				row.Latitude == nil || *row.Latitude != 35.5 || row.Longitude == nil || *row.Longitude != 139.5 ||
				!row.OnTimeDeadline.Equal(deadline) || row.WindowEndedAt != nil {
				t.Fatalf("post or author mismatch for %s: %+v", row.ID, row)
			}
		}
	}
	for _, deferred := range []bool{false, true} {
		page := read(deferred, nil)
		checkPage(page, want[:51])
		checkPage(read(deferred, &page[0]), want[1:52])
		checkPage(read(deferred, &page[49]), want[50:])
	}

	// Losing three friendships removes their posts from both pages. Deactivating
	// u3 removes 10 engagement points, but the path through u3 to u6 still counts.
	db.MustExec(`DELETE FROM friendships WHERE id IN ('bridge-68','bridge-69','bridge-70');
UPDATE users SET status='deleted' WHERE id='u3';`)
	sparse := append([]entry{{6, 19}, {7, 5}, {13, 3}}, want[6:]...)
	sparsePage := read(true, nil)
	checkPage(sparsePage, sparse[:51])
	checkPage(read(true, &sparsePage[49]), sparse[50:])

	// New third-degree paths count up to 30, and an edited post uses the new
	// payload on the first read after the write. u3 is active again but its comment is deleted.
	db.MustExec(`
INSERT INTO friendships(id,user_low_id,user_high_id)
 SELECT 'new-'||i, 'u'||i, 'u6' FROM generate_series(13,44) i;
UPDATE users SET status='active' WHERE id='u3';
UPDATE comments SET deleted_at=now() WHERE id='c3';
UPDATE posts SET caption='edited' WHERE id='fof-06';`)
	page := read(true, nil)
	if len(page) != 51 || page[0].ID != "fof-06" || page[0].Score != 46 || page[0].Caption != "edited" {
		t.Fatalf("first row after changes: %+v", page)
	}

	checkEmpty := func(wantRegion string) {
		t.Helper()
		rows := read(true, nil)
		if len(rows) != 1 || rows[0].Found || rows[0].SessRegion == nil || *rows[0].SessRegion != wantRegion {
			t.Fatalf("expected an empty-page sentinel for %s, got %+v", wantRegion, rows)
		}
	}
	h.Config.Region = "europe"
	checkEmpty("asia")
	h.Config.Region = "asia"
	db.MustExec(`UPDATE posts SET deleted_at=now() WHERE id='fof-01'`)
	checkEmpty("asia")
	db.MustExec(`UPDATE login_sessions SET active=false WHERE id='session'`)
	if rows := read(true, nil); len(rows) != 0 {
		t.Fatal("revoked session returned a row")
	}
}

func TestFriendGraphTransactionsOnPostgres(t *testing.T) {
	db := memoryTestDB(t)
	db.MustExec(`INSERT INTO friendships(id,user_low_id,user_high_id) VALUES
	    ('ab','u1','u2'), ('ba','u2','u1'), ('loop','u2','u2')`)
	installTestFriendGraph(t, db)
	installTestFriendGraph(t, db)
	check := func() {
		t.Helper()
		var different bool
		err := db.Get(&different, `WITH expected AS (
		    SELECT user_low_id AS id,user_high_id AS peer FROM friendships
		    UNION ALL SELECT user_high_id,user_low_id FROM friendships WHERE user_high_id<>user_low_id
		), actual AS (
		    SELECT v.user_id AS id, n.name AS peer, p.user_id AS decoded
		    FROM fof_vertices v CROSS JOIN unnest(v.neighbors,v.neighbor_ids) n(slot,name)
		    LEFT JOIN fof_vertices p ON p.slot=n.slot
		) SELECT EXISTS (SELECT * FROM expected EXCEPT ALL SELECT id,peer FROM actual)
		      OR EXISTS (SELECT id,peer FROM actual EXCEPT ALL SELECT * FROM expected)
		      OR EXISTS (SELECT FROM actual WHERE peer IS DISTINCT FROM decoded)`)
		if err != nil || different {
			t.Fatalf("projection differs from committed edges (%v)", err)
		}
	}
	check()
	// Different pairs contend on a shared endpoint. The second statement starts
	// before the first commits, so rebuilding from its original snapshot would
	// lose the first writer's edge. An independent reader still sees the old graph.
	first := db.MustBegin()
	defer first.Rollback()
	first.MustExec(`INSERT INTO friendships(id,user_low_id,user_high_id) VALUES ('ac','u1','u3')`)
	second := db.MustBegin()
	defer second.Rollback()
	var pid int
	if err := second.Get(&pid, "SELECT pg_backend_pid()"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := second.ExecContext(ctx, `INSERT INTO friendships(id,user_low_id,user_high_id) VALUES ('ad','u1','u4')`)
		done <- err
	}()
	for {
		var waiting bool
		if err := db.GetContext(ctx, &waiting, "SELECT cardinality(pg_blocking_pids($1)) > 0", pid); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("second writer did not wait for the common endpoint (%v)", err)
		case <-time.After(time.Millisecond):
		}
	}
	check()
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := second.Commit(); err != nil {
		t.Fatal(err)
	}
	check()
	db.MustExec(`UPDATE friendships SET user_high_id='u4' WHERE id='loop';
	    DELETE FROM friendships WHERE id IN ('ab','ac')`)
	check()
	// reset.sql restores the edges in bulk while ordinary triggers are disabled.
	// A failed reset must restore both the old projection and its slot allocator.
	db.MustExec(`BEGIN; SET LOCAL session_replication_role = replica;
	    TRUNCATE friendships; INSERT INTO friendships(id,user_low_id,user_high_id) VALUES ('temp','u5','u6');
	    ROLLBACK`)
	check()
	db.MustExec(`BEGIN; SET LOCAL session_replication_role = replica; TRUNCATE friendships;
	    INSERT INTO fof_graph_active VALUES (true);
	    INSERT INTO friendships(id,user_low_id,user_high_id) VALUES ('x','u5','u6'),('y','u6','u7'); COMMIT`)
	check()
	var count int
	if err := db.Get(&count, "SELECT count(*) FROM fof_vertices"); err != nil || count != 3 {
		t.Fatalf("vertices after reset = %d (%v)", count, err)
	}
	// An older reset truncates and restores friendships under replica mode but
	// does not opt in. Its subsequent normal writers must not maintain our index.
	db.MustExec(`BEGIN; SET LOCAL session_replication_role = replica; TRUNCATE friendships;
	    INSERT INTO friendships(id,user_low_id,user_high_id) VALUES ('old','u1','u2'); COMMIT;
	    INSERT INTO friendships(id,user_low_id,user_high_id) VALUES ('live','u2','u3')`)
	if err := db.Get(&count, "SELECT count(*) FROM fof_vertices"); err != nil || count != 0 {
		t.Fatalf("inactive graph received legacy writes: %d (%v)", count, err)
	}
	installTestFriendGraph(t, db)
	check()
}
