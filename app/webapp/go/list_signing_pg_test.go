package isureal

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

func TestRoundTripHandlersOnPostgresListPages(t *testing.T) {
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
	h := &HandlerContext{DB: db, Config: &Config{Region: "asia"}}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	// Authentication is covered by the existing roundtrip tests. Use a fixed
	// authenticated viewer here to exercise the page/sentinel and signing work.
	auth := func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Set("session", &Session{UserID: "viewer", Region: "asia"})
			return next(c)
		}
	}
	e.GET("/api/feeds/:feed", getFeedHandler(h), auth)
	e.GET("/api/posts/:postId/comments", getCommentsHandler(h), auth)
	e.GET("/api/posts/:postId/realmojis", getRealMojisHandler(h), auth)
	e.GET("/api/relationships/:kind", getRelationshipsHandler(h), auth)
	e.GET("/api/users", searchUsersHandler(h), auth)
	for _, n := range []int{0, 49, 50, 51} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			for _, q := range []string{
				`TRUNCATE users, posting_windows, user_search_norms, user_search_dirty, friendships CASCADE`,
				`INSERT INTO users(id,username,secret_key_hash,display_name,region) VALUES('viewer','viewer',repeat('ab',32),'Viewer','asia')`,
				`INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline) VALUES('window','asia',CURRENT_DATE,now()-interval '1 hour',now()+interval '1 hour')`,
				`INSERT INTO posts(id,author_id,posting_window_id,image_id,visibility,location_kind,posted_at) VALUES('viewers-post','viewer','window','viewer-image','public','none',now())`,
				fmt.Sprintf(`INSERT INTO users(id,username,secret_key_hash,display_name,bio,profile_image_id,region)
				 SELECT 'reader-'||lpad(i::text,2,'0'),'reader'||lpad(i::text,2,'0'),repeat('ab',32),'Reader','private bio','profile-'||i,'asia' FROM generate_series(1,%d) i`, n),
				`INSERT INTO posts(id,author_id,posting_window_id,image_id,visibility,location_kind,posted_at)
				 SELECT 'post-'||id,id,'window','image-'||id,'public','none',now() FROM users WHERE id<>'viewer'`,
				`INSERT INTO friendships(id,user_low_id,user_high_id) SELECT 'friend-'||id,id,'viewer' FROM users WHERE id<>'viewer'`,
				`INSERT INTO comments(id,post_id,author_id,body,created_at) SELECT 'comment-'||id,'viewers-post',id,'body',now() FROM users WHERE id<>'viewer'`,
				`INSERT INTO realmojis(id,post_id,reactor_id,image_id,reaction_type,created_at) SELECT 'realmoji-'||id,'viewers-post',id,'reaction-'||id,'like',now() FROM users WHERE id<>'viewer'`,
				`INSERT INTO user_search_norms(user_id,username_norm,display_name,display_name_norm) SELECT id,username,display_name,lower(display_name) FROM users`,
			} {
				if _, err := db.Exec(q); err != nil {
					t.Fatalf("seed %s: %+v", q, err)
				}
			}
			for _, target := range []string{"/api/feeds/public", "/api/posts/viewers-post/comments", "/api/posts/viewers-post/realmojis", "/api/relationships/followers", "/api/relationships/friends", "/api/users?q=reader"} {
				rec := serveRoundTrip(e, "", http.MethodGet, target, "")
				if rec.Code != http.StatusOK {
					t.Fatalf("%s: %d %s", target, rec.Code, rec.Body.String())
				}
				var page struct {
					Items   []json.RawMessage `json:"items"`
					HasMore bool              `json:"hasMore"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
					t.Fatal(err)
				}
				want := min(n, 50)
				if strings.Contains(target, "followers") {
					want = 0
				}
				wantMore := n > 50 && !strings.Contains(target, "followers")
				if len(page.Items) != want || page.HasMore != wantMore {
					t.Fatalf("%s: items=%d hasMore=%v; want %d %v", target, len(page.Items), page.HasMore, want, wantMore)
				}
				if n > 0 && !strings.Contains(target, "followers") && !strings.Contains(rec.Body.String(), "sig=") {
					t.Fatalf("%s: missing image signatures", target)
				}
				if strings.Contains(target, "/api/users?") && strings.Contains(rec.Body.String(), "private bio") {
					t.Fatalf("search leaked private bio")
				}
			}
		})
	}
}
