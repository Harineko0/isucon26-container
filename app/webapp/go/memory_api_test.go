package isureal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// Run against a disposable local PostgreSQL 16:
// ISUREAL_TEST_DATABASE_URL=postgres://postgres:test@127.0.0.1:55427/postgres?sslmode=disable go test -run TestMemories -v
func memoryTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("ISUREAL_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DATABASE_URL is not set")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" {
		t.Fatal("test database must be local")
	}
	admin, err := sqlx.Connect("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	schema := fmt.Sprintf("memory_test_%d", time.Now().UnixNano())
	admin.MustExec("CREATE SCHEMA " + schema)
	t.Cleanup(func() { admin.MustExec("DROP SCHEMA " + schema + " CASCADE") })
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := sqlx.Connect("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	schemaSQL, err := os.ReadFile("../sql/001-schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	db.MustExec(string(schemaSQL))
	db.MustExec(`CREATE INDEX posts_author_window ON posts(author_id, posting_window_id);
 CREATE INDEX login_sessions_token_hash ON login_sessions(token_hash);
 INSERT INTO users(id,username,secret_key_hash,display_name,bio,profile_image_id,region,account_type)
 SELECT 'u'||i, 'user'||i, lpad(to_hex(i),64,'0'), '表示名 '||i, 'bio '||i,
        CASE WHEN i % 2 = 0 THEN 'profile-'||i END, 'asia',
        CASE WHEN i % 3 = 0 THEN 'certified_public' ELSE 'regular' END
 FROM generate_series(1,70) i;
 INSERT INTO posting_windows(id,region,local_date,starts_at,on_time_deadline,ended_at)
 SELECT 'w'||i, (ARRAY['asia','europe','america'])[1+(i-1)%3], date '2026-10-01'+(i-1)/3,
        timestamptz '2026-10-01 00:00:00Z'+((i-1)/3)*interval '1 day',
        timestamptz '2026-10-01 00:00:00Z'+((i-1)/3)*interval '1 day'+interval '2 minutes',
        timestamptz '2026-10-01 00:00:00Z'+((i-1)/3)*interval '1 day'+interval '10 minutes'
 FROM generate_series(1,60) i;
 INSERT INTO posts(id,author_id,posting_window_id,image_id,caption,visibility,location_kind,latitude,longitude,locality,posted_at,deleted_at)
 SELECT 'p'||lpad(i::text,3,'0'), 'u1', w.id, 'img'||i, 'caption '||i,
        (ARRAY['public','friends','friends_of_friends'])[1+i%3],
        (ARRAY['none','exact','approximate'])[1+i%3],
        CASE WHEN i%3=1 THEN 35.6 END, CASE WHEN i%3=1 THEN 139.7 END,
        CASE WHEN i%3=2 THEN '東京' END, w.starts_at+interval '1 minute',
        CASE WHEN i>=56 THEN w.starts_at END
 FROM generate_series(1,60) i JOIN posting_windows w ON w.id='w'||i;
 INSERT INTO realmojis(id,post_id,reactor_id,image_id,reaction_type,created_at,replaced_at,deleted_at)
 SELECT 'r'||lpad(i::text,3,'0'), 'p055', 'u'||(i+1), 'reaction-img'||i, 'smile',
        timestamptz '2026-10-20 00:00:00Z' + (i/3)*interval '1 second',
        CASE WHEN i=2 THEN now() END, CASE WHEN i=3 THEN now() END
 FROM generate_series(1,65) i;
 INSERT INTO realmojis(id,post_id,reactor_id,image_id,reaction_type,created_at)
 SELECT 'other-'||id, 'p054', reactor_id, image_id, reaction_type, created_at
 FROM realmojis WHERE post_id='p055';
 INSERT INTO comments(id,post_id,author_id,body,created_at,deleted_at)
 SELECT 'c'||lpad(i::text,3,'0'), 'p055', 'u'||(i+1), 'コメント '||i,
        timestamptz '2026-10-20 00:00:00Z' + (i/3)*interval '1 second', CASE WHEN i=3 THEN now() END
 FROM generate_series(1,65) i;
 INSERT INTO comments(id,post_id,author_id,body,created_at)
 SELECT 'other-'||id, 'p054', author_id, body, created_at FROM comments WHERE post_id='p055';
 UPDATE users SET status='deleted' WHERE id='u5';`)
	return db
}

func memoryContext() *echo.Context {
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/api/memories", nil), httptest.NewRecorder())
	c.Set("session", &Session{UserID: "u1", Region: "asia"})
	return c
}

func TestMemoriesMatchesLegacy(t *testing.T) {
	db := memoryTestDB(t)
	h := &HandlerContext{DB: db, Config: &Config{Region: "asia"}}
	month := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cursor := month.Add(14*24*time.Hour + time.Minute)
	cases := []struct {
		name       string
		start, end time.Time
		before     *time.Time
		id         string
	}{
		{"month with hasMore", month, month.AddDate(0, 1, 0), nil, ""},
		{"day with interactions", month.AddDate(0, 0, 18), month.AddDate(0, 0, 19), nil, ""},
		{"tied timestamp cursor", month, month.AddDate(0, 1, 0), &cursor, "p044"},
		{"empty period", month.AddDate(1, 0, 0), month.AddDate(1, 1, 0), nil, ""},
	}
	compare := func(t *testing.T, start, end time.Time, before *time.Time, id string) getMemoriesResponse {
		t.Helper()
		var got getMemoriesResponse
		for range 3 {
			want, err := legacyListMemories(context.Background(), db, "u1", start, end, before, id)
			if err != nil {
				t.Fatal(err)
			}
			got, err = listMemories(memoryContext(), h, start, end, before, id)
			if err != nil {
				t.Fatal(err)
			}
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)
			if !bytes.Equal(gotJSON, wantJSON) {
				for i := 0; i < len(gotJSON) && i < len(wantJSON); i++ {
					if gotJSON[i] != wantJSON[i] {
						t.Logf("difference at %d: got %s want %s", i, gotJSON[max(0, i-50):min(len(gotJSON), i+150)], wantJSON[max(0, i-50):min(len(wantJSON), i+150)])
						break
					}
				}
			}
			if bytes.Equal(gotJSON, wantJSON) {
				return got
			}
		}
		t.Fatal("Memories response differs from legacy (including order, signatures, profile, and empty arrays)")
		return got
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := compare(t, tc.start, tc.end, tc.before, tc.id)
			if tc.name == "month with hasMore" {
				if len(got.Items) != 50 || !got.HasMore {
					t.Fatalf("page: %d, more=%v", len(got.Items), got.HasMore)
				}
				for _, item := range got.Items[:2] {
					if len(item.RealMojis) != 50 || len(item.Comments) != 50 {
						t.Fatalf("interaction limits for post %s", item.ID)
					}
				}
				data, _ := json.Marshal(got)
				if strings.Contains(string(data), "SigningKey") || strings.Contains(string(data), "secret_key_hash") {
					t.Fatal("private key leaked")
				}
			}
		})
	}
	t.Run("read after write", func(t *testing.T) {
		// Values that require escaping in JSON still round-trip verbatim, and
		// the binary timestamptz field preserves all six fractional digits.
		db.MustExec(`UPDATE comments SET body=$1, created_at=$2 WHERE id='c002'`,
			"改行\n\"引用\"\\🦭", time.Date(2026, 10, 1, 1, 2, 3, 456789000, time.FixedZone("test", 9*3600)))
		db.MustExec(`UPDATE users SET display_name='updated', profile_image_id='new-profile' WHERE id='u1';
   UPDATE users SET display_name='new reactor', profile_image_id='new-reactor-profile' WHERE id='u66';
   UPDATE comments SET deleted_at=now() WHERE id='c001';
   UPDATE realmojis SET replaced_at=now() WHERE id='r065';
   UPDATE users SET status='deleted' WHERE id='u65';
   UPDATE posts SET caption='changed caption' WHERE id='p055';`)
		compare(t, month, month.AddDate(0, 1, 0), nil, "")
		db.MustExec(`UPDATE posts SET deleted_at=now() WHERE id='p055'`)
		compare(t, month, month.AddDate(0, 1, 0), nil, "")
	})
}

func TestMemoriesSessionAndErrorPriority(t *testing.T) {
	db := memoryTestDB(t)
	h := &HandlerContext{DB: db, Config: &Config{Region: "asia"}}
	token := "memory-session-token"
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	db.MustExec(`INSERT INTO login_sessions(id,user_id,token_hash) VALUES ('session','u1',$1)`, hash)
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.GET("/api/memories", getMemoriesHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	e.GET("/api/memories/:date", getMemoriesByDateHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	for _, tc := range []struct {
		name, path, status, region string
		active, warm               bool
		want                       int
	}{
		{"cold session", "/api/memories?month=2026-10", "active", "asia", true, false, 200},
		{"warm session", "/api/memories?month=2026-10", "active", "asia", true, true, 200},
		{"day route", "/api/memories/2026-10-19", "active", "asia", true, true, 200},
		{"empty authenticated page", "/api/memories?month=2027-10", "active", "asia", true, true, 200},
		{"bad month", "/api/memories?month=no", "active", "asia", true, true, 400},
		{"bad cursor", "/api/memories?month=2026-10&beforeId=x", "active", "asia", true, true, 400},
		{"revoked session", "/api/memories?month=2026-10", "active", "asia", false, true, 401},
		{"revoked beats bad input", "/api/memories?month=no", "active", "asia", false, true, 401},
		{"region mismatch", "/api/memories?month=2026-10", "active", "europe", true, true, 403},
		{"mismatch beats bad input", "/api/memories?month=no", "active", "europe", true, true, 403},
		{"deleted user", "/api/memories?month=2026-10", "deleted", "asia", true, true, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db.MustExec(`UPDATE login_sessions SET active=$1`, tc.active)
			db.MustExec(`UPDATE users SET status=$1,region=$2 WHERE id='u1'`, tc.status, tc.region)
			knownSessions.Delete(hash)
			if tc.warm {
				knownSessions.Store(hash, Session{UserID: "u1", TokenHash: hash, Region: "asia"})
			}
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
	knownSessions.Delete(hash)
}

// Baseline implementation is retained as a response oracle for the SQL rewrite.
func legacyListMemories(ctx context.Context, db sqlx.QueryerContext, userID string, start, end time.Time, beforeAt *time.Time, beforeID string) (getMemoriesResponse, error) {
	var response getMemoriesResponse
	if (beforeAt == nil) != (beforeID == "") {
		return response, NewAPIError(http.StatusBadRequest, "invalid_pagination")
	}
	posts, err := selectPostsWithAuthorFrom(ctx, db, memoriesFrom, `
  AND p.author_id = $1 AND w.local_date >= $2 AND w.local_date < $3
  AND ($4::timestamptz IS NULL OR (p.posted_at, p.id) < ($4, $5))
  ORDER BY p.posted_at DESC, p.id DESC LIMIT 51
 `, userID, start, end, beforeAt, beforeID)
	if err != nil {
		return response, err
	}
	if len(posts) > 50 {
		response.HasMore = true
		posts = posts[:50]
	}
	postIDs := make([]string, len(posts))
	for i := range posts {
		postIDs[i] = posts[i].ID
	}
	reactions, comments, err := legacyLoadMemoryInteractions(ctx, db, postIDs)
	if err != nil {
		return response, err
	}
	plan := newSignaturePlan()
	for _, post := range posts {
		plan.add(post.Author, post.ImageID)
		plan.addProfile(post.Author)
	}
	for _, reaction := range reactions {
		plan.add(reaction.Reactor, reaction.ImageID)
		plan.addProfile(reaction.Reactor)
	}
	for _, comment := range comments {
		plan.addProfile(comment.Author)
	}
	plan.run()
	items := make([]memoryPostResponse, 0, len(posts))
	itemByPostID := make(map[string]*memoryPostResponse, len(posts))
	for _, post := range posts {
		items = append(items, memoryPostResponse{
			createPostResponse: newPostResponse(post.Post, post.Author),
			RealMojis:          make([]replaceRealMojiResponse, 0),
			Comments:           make([]createCommentResponse, 0),
		})
	}
	for i := range items {
		itemByPostID[items[i].ID] = &items[i]
	}
	for _, reaction := range reactions {
		item := itemByPostID[reaction.PostID]
		if len(item.RealMojis) < 50 {
			item.RealMojis = append(item.RealMojis, newRealMojiResponse(reaction.RealMoji, reaction.Reactor))
		}
	}
	for _, comment := range comments {
		item := itemByPostID[comment.PostID]
		if len(item.Comments) < 50 {
			item.Comments = append(item.Comments, newCommentResponse(comment.Comment, comment.Author))
		}
	}
	response.Items = items
	return response, nil
}

func legacyLoadMemoryInteractions(ctx context.Context, db sqlx.QueryerContext, postIDs []string) ([]RealMojiWithReactor, []CommentWithAuthor, error) {
	if len(postIDs) == 0 {
		return nil, nil, nil
	}
	// RealMoji とコメントは独立なので並行に取る。
	// 投稿数(1〜50)ごとに文が変わると接続ごとに Prepare の往復が増えるので、= ANY($1) の固定の SQL にする。
	var comments []CommentWithAuthor
	var commentsErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		comments, commentsErr = selectCommentsWithAuthor(ctx, db, `AND c.post_id = ANY($1) ORDER BY c.created_at, c.id`, postIDs)
	}()
	reactions, err := selectCurrentRealMojis(ctx, db, `AND r.post_id = ANY($1) ORDER BY r.created_at DESC, r.id DESC`, postIDs)
	<-done
	if err != nil {
		return nil, nil, err
	}
	if commentsErr != nil {
		return nil, nil, commentsErr
	}
	return reactions, comments, nil
}
