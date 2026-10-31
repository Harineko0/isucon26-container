package isureal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// TestRoundTripHandlersOnPostgres は #17 で 1 文にしたハンドラを本物の PostgreSQL 16 で動かし、SQL
// (005-roundtrips.sql の isureal_update_post_returning を含む) と 401/403/404 の順序、更新直後の読み取りを確かめる。
// ISUREAL_TEST_DSN (001/002/007-image-location/004/005 を流しただけの空の DB) が無ければ飛ばす。
// 手順は sql/tests/roundtrips.sh。
func TestRoundTripHandlersOnPostgres(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set (see app/webapp/sql/tests/roundtrips.sh)")
	}
	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db := sqlx.NewDb(stdlib.OpenDB(*connConfig), "pgx")
	defer db.Close()
	ctx := context.Background()

	hash := func(token string) string {
		sum := sha256.Sum256([]byte(token))
		return hex.EncodeToString(sum[:])
	}
	key := strings.Repeat("cd", sha256.Size)
	seed := []string{
		`TRUNCATE users, login_sessions, posting_windows, posts, comments, friendships CASCADE`,
		`INSERT INTO users (id, username, secret_key_hash, display_name, region) VALUES
		   ('u1', 'alice', '` + key + `', 'Alice', 'europe'),
		   ('u2', 'bob', '` + key + `', 'Bob', 'europe'),
		   ('u3', 'carol', '` + key + `', 'Carol', 'asia'),
		   ('u4', 'dave', '` + key + `', 'Dave', 'europe')`,
		`INSERT INTO login_sessions (id, user_id, token_hash) VALUES
		   ('s1', 'u1', '` + hash("t1") + `'), ('s2', 'u2', '` + hash("t2") + `'),
		   ('s3', 'u3', '` + hash("t3") + `'), ('s4', 'u4', '` + hash("t4") + `')`,
		`INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline) VALUES
		   ('w1', 'europe', current_date, now() - interval '1 minute', now() + interval '1 minute')`,
		`INSERT INTO posts (id, author_id, posting_window_id, image_id, caption, visibility, location_kind, posted_at) VALUES
		   ('p1', 'u1', 'w1', 'i1', 'before', 'friends', 'none', now()),
		   ('p2', 'u2', 'w1', 'i2', 'bob', 'public', 'none', now()),
		   ('p4', 'u4', 'w1', 'i4', 'dave', 'public', 'none', now())`,
		`INSERT INTO friendships (id, user_low_id, user_high_id) VALUES ('f1', 'u1', 'u2')`,
		`INSERT INTO comments (id, post_id, author_id, body, created_at) VALUES ('c1', 'p1', 'u2', 'hi', now())`,
	}
	for _, statement := range seed {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	h := &HandlerContext{DB: db, Config: &Config{Region: "europe"}}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	auth := []echo.MiddlewareFunc{SessionMiddleware(h, true), RegionMiddleware(h)}
	e.GET("/api/posts/:postId", getPostHandler(h), auth...)
	e.PATCH("/api/posts/:postId", updatePostHandler(h), auth...)
	e.DELETE("/api/posts/:postId", deletePostHandler(h), auth...)
	e.DELETE("/api/comments/:commentId", deleteCommentHandler(h), auth...)
	e.GET("/api/me/export", exportPostsHandler(h), auth...)
	imageDataStore.put("i1", []byte("jpeg-bytes"))
	t.Cleanup(func() { imageDataStore.reset() })

	// 各トークンで 1 回 GET して覚えさせ (2 回目以降の deferred 経路にする)、以降は 1 文の経路を通す。
	for _, token := range []string{"t1", "t2", "t3", "t4"} {
		serveRoundTrip(e, token, http.MethodGet, "/api/posts/p2", "")
	}

	steps := []struct {
		name, token, method, target, body string
		wantStatus                        int
		wantCaption                       string
	}{
		{"author reads own post", "t1", http.MethodGet, "/api/posts/p1", "", http.StatusOK, "before"},
		{"friend who posted today reads friends-only post", "t2", http.MethodGet, "/api/posts/p1", "", http.StatusOK, "before"},
		{"non-friend cannot see friends-only post", "t4", http.MethodGet, "/api/posts/p1", "", http.StatusNotFound, ""},
		{"other region is 403 before 404", "t3", http.MethodGet, "/api/posts/p1", "", http.StatusForbidden, ""},
		{"invalid session is 401", "nope", http.MethodGet, "/api/posts/p1", "", http.StatusUnauthorized, ""},
		{"missing post is 404", "t1", http.MethodGet, "/api/posts/none", "", http.StatusNotFound, ""},

		{"author edits caption and gets the updated post", "t1", http.MethodPatch, "/api/posts/p1", `{"caption":"after"}`, http.StatusOK, "after"},
		{"read after write", "t2", http.MethodGet, "/api/posts/p1", "", http.StatusOK, "after"},
		{"non-author cannot edit", "t2", http.MethodPatch, "/api/posts/p1", `{"caption":"x"}`, http.StatusForbidden, ""},
		{"other region cannot edit", "t3", http.MethodPatch, "/api/posts/p1", `{"caption":"x"}`, http.StatusForbidden, ""},
		{"edit of missing post is 404", "t1", http.MethodPatch, "/api/posts/none", `{"caption":"x"}`, http.StatusNotFound, ""},
		{"invalid session beats 400", "nope", http.MethodPatch, "/api/posts/p1", `{}`, http.StatusUnauthorized, ""},
		{"rejected edits changed nothing", "t1", http.MethodGet, "/api/posts/p1", "", http.StatusOK, "after"},
		{"visibility edit keeps caption", "t1", http.MethodPatch, "/api/posts/p1", `{"visibility":"public"}`, http.StatusOK, "after"},
		{"now public: non-friend sees it", "t4", http.MethodGet, "/api/posts/p1", "", http.StatusOK, "after"},

		{"other region cannot delete a comment", "t3", http.MethodDelete, "/api/comments/c1", "", http.StatusForbidden, ""},
		{"stranger cannot delete a comment", "t4", http.MethodDelete, "/api/comments/c1", "", http.StatusForbidden, ""},
		{"post author deletes a comment", "t1", http.MethodDelete, "/api/comments/c1", "", http.StatusNoContent, ""},
		{"deleted comment is 404", "t1", http.MethodDelete, "/api/comments/c1", "", http.StatusNotFound, ""},

		{"non-author cannot delete a post", "t1", http.MethodDelete, "/api/posts/p2", "", http.StatusForbidden, ""},
		{"other region cannot delete a post", "t3", http.MethodDelete, "/api/posts/p2", "", http.StatusForbidden, ""},
		{"author deletes a post", "t2", http.MethodDelete, "/api/posts/p2", "", http.StatusNoContent, ""},
		{"deleted post is gone", "t2", http.MethodGet, "/api/posts/p2", "", http.StatusNotFound, ""},
		{"deleted post cannot be deleted again", "t2", http.MethodDelete, "/api/posts/p2", "", http.StatusNotFound, ""},

		{"export own posts in one statement", "t1", http.MethodGet, "/api/me/export", "", http.StatusOK, ""},
		{"export empty posts in one statement", "t2", http.MethodGet, "/api/me/export", "", http.StatusOK, ""},
		{"export other region is 403", "t3", http.MethodGet, "/api/me/export", "", http.StatusForbidden, ""},
		{"export invalid session is 401", "nope", http.MethodGet, "/api/me/export", "", http.StatusUnauthorized, ""},
	}
	for _, step := range steps {
		rec := serveRoundTrip(e, step.token, step.method, step.target, step.body)
		if rec.Code != step.wantStatus {
			t.Fatalf("%s: status = %d (%s), want %d", step.name, rec.Code, rec.Body.String(), step.wantStatus)
		}
		if step.wantCaption == "" {
			continue
		}
		var body struct {
			ID       string `json:"id"`
			Caption  string `json:"caption"`
			ImageURL string `json:"imageUrl"`
			Author   struct {
				ID string `json:"id"`
			} `json:"author"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if body.Caption != step.wantCaption || body.Author.ID != "u1" || !strings.HasPrefix(body.ImageURL, "/api/images/i1?") {
			t.Fatalf("%s: body = %s", step.name, rec.Body.String())
		}
	}
}
