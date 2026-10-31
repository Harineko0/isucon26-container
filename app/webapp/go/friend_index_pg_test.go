package isureal

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

func TestRoundTripHandlersOnPostgresFriendIndexEquivalence(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db := sqlx.NewDb(stdlib.OpenDB(*cfg), "pgx")
	defer db.Close()
	for _, q := range []string{
		`TRUNCATE users, posting_windows, user_search_norms, user_search_dirty CASCADE`,
		`INSERT INTO users(id,username,secret_key_hash,display_name,region,status)
		 SELECT 'u'||lpad(i::text,3,'0'),'user'||i,repeat('ab',32),'User','asia',CASE WHEN i%13=0 THEN 'deleted' ELSE 'active' END FROM generate_series(1,60) i`,
		`INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline) VALUES('w','asia',CURRENT_DATE,now()-interval '1 hour',now())`,
		`INSERT INTO posts(id,author_id,posting_window_id,image_id,visibility,location_kind,posted_at)
		 SELECT 'p'||id,id,'w','image'||id,CASE WHEN right(id,1)::int%3=0 THEN 'friends' WHEN right(id,1)::int%3=1 THEN 'public' ELSE 'friends_of_friends' END,'none',now()-substring(id,2)::int*interval '1 second' FROM users`,
		// Include self edges too: UNION ALL must retain the old OR's multiplicity.
		`INSERT INTO friendships(id,user_low_id,user_high_id) SELECT a.id||b.id,a.id,b.id FROM users a JOIN users b ON a.id<=b.id WHERE (substring(a.id,2)::int+substring(b.id,2)::int)%4=0`,
		`INSERT INTO follows(id,follower_id,followee_id) SELECT a.id||b.id,a.id,b.id FROM users a JOIN users b ON a.id<>b.id WHERE substring(b.id,2)::int%3=0`,
		`INSERT INTO block_relations(id,blocker_id,blocked_id) VALUES('block1','u001','u009'),('block2','u014','u002')`,
		`INSERT INTO comments(id,post_id,author_id,body,created_at) SELECT p.id||u.id,p.id,u.id,'hi',now() FROM posts p JOIN users u ON substring(u.id,2)::int%5=0`,
		`INSERT INTO realmojis(id,post_id,reactor_id,image_id,reaction_type,created_at) SELECT p.id||u.id,p.id,u.id,'rimage','like',now() FROM posts p JOIN users u ON substring(u.id,2)::int%7=0`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	oldFriends := `    SELECT CASE WHEN user_low_id = $1 THEN user_high_id ELSE user_low_id END AS id
    FROM friendships WHERE user_low_id = $1 OR user_high_id = $1`
	newFriends := `    SELECT user_high_id AS id FROM friendships WHERE user_low_id = $1
    UNION ALL
    SELECT user_low_id AS id FROM friendships WHERE user_high_id = $1 AND user_low_id <> $1`
	oldJoin := `    JOIN friendships fs ON fs.user_low_id = a.author_id OR fs.user_high_id = a.author_id
    JOIN mutual m ON m.id = CASE WHEN fs.user_low_id = a.author_id THEN fs.user_high_id ELSE fs.user_low_id END`
	newJoin := `    CROSS JOIN LATERAL (
      SELECT user_high_id AS id FROM friendships WHERE user_low_id = a.author_id
      UNION ALL
      SELECT user_low_id AS id FROM friendships WHERE user_high_id = a.author_id AND user_low_id <> a.author_id
    ) neighbors
    JOIN mutual m ON m.id = neighbors.id`
	for kind, query := range feedQueries {
		old := strings.ReplaceAll(strings.ReplaceAll(query, newFriends, oldFriends), newJoin, oldJoin)
		if kind != "public" && old == query {
			t.Fatalf("%s: comparison did not replace query", kind)
		}
		for _, viewer := range []string{"u001", "u002", "u004"} {
			for _, cursor := range []bool{false, true} {
				var at *time.Time
				id := ""
				if cursor {
					ts := time.Now().Add(-25 * time.Second)
					at = &ts
					id = "pu025"
				}
				args := []any{viewer, at, id}
				if kind == "friends-of-friends" {
					var score *int64
					if cursor {
						x := int64(100)
						score = &x
					}
					args = append(args, score)
				}
				var got, want []string
				for i, q := range []string{query, old} {
					var rows []string
					if err := db.Select(&rows, `SELECT row_to_json(x)::text FROM (`+q+`) x ORDER BY x.id`, args...); err != nil {
						t.Fatalf("%s: %v", kind, err)
					}
					if i == 0 {
						got = rows
					} else {
						want = rows
					}
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s %s cursor=%v differs: %s", kind, viewer, cursor, fmt.Sprint(got, want))
				}
			}
		}
	}
}
