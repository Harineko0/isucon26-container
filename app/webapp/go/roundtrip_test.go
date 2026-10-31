package isureal

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// #17: EU/US は DB まで往復 ~100ms なので、1 リクエストで DB に投げる文の数 (= 往復) と、
// 同時に握る接続の数を数える。recordingConnector は文を記録し、respond が返す行をそのまま返すだけの
// ドライバ (Prepare は使わせない: QueryerContext で 1 文 = 1 回の QueryContext になる)。

type recordedQuery struct {
	query string
	args  []driver.NamedValue
}

type recordingConnector struct {
	respond func(query string) (columns []string, rows [][]driver.Value)

	mu       sync.Mutex
	queries  []recordedQuery
	opened   int
	inUse    int
	maxInUse int
}

func (c *recordingConnector) Connect(context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opened++
	return &recordingConn{c: c}, nil
}

func (c *recordingConnector) Driver() driver.Driver { return nil }

func (c *recordingConnector) recorded() []recordedQuery {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recordedQuery(nil), c.queries...)
}

type recordingConn struct{ c *recordingConnector }

func (*recordingConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not expected")
}
func (*recordingConn) Close() error              { return nil }
func (*recordingConn) Begin() (driver.Tx, error) { return nil, errors.New("transactions not expected") }

func (conn *recordingConn) QueryContext(_ context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c := conn.c
	c.mu.Lock()
	c.queries = append(c.queries, recordedQuery{query: query, args: args})
	c.inUse++
	if c.inUse > c.maxInUse {
		c.maxInUse = c.inUse
	}
	c.mu.Unlock()
	// 並行に投げる実装なら、ここで重なって maxInUse が 2 以上になる (往復 ~100ms の模擬)。
	time.Sleep(20 * time.Millisecond)
	columns, rows := c.respond(query)
	c.mu.Lock()
	c.inUse--
	c.mu.Unlock()
	return &recordingRows{columns: columns, rows: rows}, nil
}

type recordingRows struct {
	columns []string
	rows    [][]driver.Value
}

func (r *recordingRows) Columns() []string { return r.columns }
func (r *recordingRows) Close() error      { return nil }
func (r *recordingRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

const roundTripToken = "roundtrip-test-token"

func roundTripTokenHash() string {
	sum := sha256.Sum256([]byte(roundTripToken))
	return hex.EncodeToString(sum[:])
}

// roundTripServer は本番と同じミドルウェア (SessionMiddleware → RegionMiddleware) で 1 ルートを組み、
// 利用者 u1 (europe) のセッションを覚えている状態 (2 回目以降のリクエスト) にする。
func roundTripServer(t *testing.T, conn *recordingConnector, method, path string, handler func(*HandlerContext) echo.HandlerFunc) *echo.Echo {
	t.Helper()
	db := sqlx.NewDb(sql.OpenDB(conn), "pgx")
	t.Cleanup(func() { db.Close() })
	h := &HandlerContext{DB: db, Config: &Config{Region: "europe"}}
	hash := roundTripTokenHash()
	knownSessions.Store(hash, Session{UserID: "u1", TokenHash: hash, Region: "europe"})
	t.Cleanup(func() { knownSessions.Delete(hash) })
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.Add(method, path, handler(h), SessionMiddleware(h, true), RegionMiddleware(h))
	return e
}

func serveRoundTrip(e *echo.Echo, token, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
	if body != "" {
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestDeleteWritesCheckSessionInTheSameStatement(t *testing.T) {
	routes := []struct {
		name     string
		path     string
		target   string
		function string
		handler  func(*HandlerContext) echo.HandlerFunc
	}{
		{"comment", "/api/comments/:commentId", "/api/comments/c1", "isureal_delete_comment", deleteCommentHandler},
		{"realmoji", "/api/realmojis/:realMojiId", "/api/realmojis/r1", "isureal_delete_realmoji", deleteRealMojiHandler},
		{"post", "/api/posts/:postId", "/api/posts/p1", "isureal_delete_post", deletePostHandler},
	}
	cases := []struct {
		name       string
		rows       [][]driver.Value // (sess_region, result)
		wantStatus int
	}{
		{"deleted", [][]driver.Value{{"europe", "ok"}}, http.StatusNoContent},
		{"not found", [][]driver.Value{{"europe", "not_found"}}, http.StatusNotFound},
		// リージョンが違うと関数は呼ばれず result は NULL。
		{"region mismatch", [][]driver.Value{{"asia", nil}}, http.StatusForbidden},
		// セッションが無効なら 0 行。
		{"invalid session", nil, http.StatusUnauthorized},
	}
	for _, route := range routes {
		for _, tc := range cases {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				conn := &recordingConnector{respond: func(string) ([]string, [][]driver.Value) {
					return []string{"sess_region", "result"}, tc.rows
				}}
				e := roundTripServer(t, conn, http.MethodDelete, route.path, route.handler)
				rec := serveRoundTrip(e, roundTripToken, http.MethodDelete, route.target, "")
				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), tc.wantStatus)
				}
				queries := conn.recorded()
				// 以前は SessionMiddleware の lookupSession + 関数の 2 往復。
				if len(queries) != 1 {
					t.Fatalf("%d statements, want 1: %v", len(queries), queries)
				}
				q := queries[0].query
				if !strings.Contains(q, "login_sessions") || !strings.Contains(q, route.function+"(s.sess_user_id, $1)") {
					t.Fatalf("session check and %s are not in one statement: %s", route.function, q)
				}
				if args := queries[0].args; len(args) != 3 || args[1].Value != roundTripTokenHash() || args[2].Value != "europe" {
					t.Fatalf("args = %v, want (id, token hash, region)", args)
				}
			})
		}
	}
}

func updatePostReturningRow(sessRegion, result any, caption string) []driver.Value {
	posted := time.Date(2026, 10, 31, 9, 0, 5, 0, time.UTC)
	deadline := time.Date(2026, 10, 31, 9, 2, 0, 0, time.UTC)
	key := strings.Repeat("ab", sha256.Size)
	return []driver.Value{sessRegion, result,
		"p1", "u1", "w1", "i1", caption, "friends", "none", nil, nil, nil, posted, deadline, nil,
		"u1", "alice", key, "Alice", "", nil, "europe", "regular", "active"}
}

var updatePostReturningColumnNames = []string{"sess_region", "result",
	"id", "author_id", "posting_window_id", "image_id", "caption", "visibility", "location_kind", "latitude", "longitude", "locality",
	"posted_at", "on_time_deadline", "window_ended_at",
	"author.id", "author.username", "author.secret_key_hash", "author.display_name", "author.bio", "author.profile_image_id",
	"author.region", "author.account_type", "author.status"}

func TestUpdatePostIsOneStatement(t *testing.T) {
	cases := []struct {
		name       string
		rows       [][]driver.Value
		wantStatus int
	}{
		{"updated", [][]driver.Value{updatePostReturningRow("europe", "ok", "new caption")}, http.StatusOK},
		{"not found", [][]driver.Value{updatePostReturningRow("europe", "not_found", "")}, http.StatusNotFound},
		{"not editable", [][]driver.Value{updatePostReturningRow("europe", "forbidden", "")}, http.StatusForbidden},
		{"region mismatch", [][]driver.Value{updatePostReturningRow("asia", "", "")}, http.StatusForbidden},
		{"invalid session", nil, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &recordingConnector{respond: func(string) ([]string, [][]driver.Value) {
				return updatePostReturningColumnNames, tc.rows
			}}
			e := roundTripServer(t, conn, http.MethodPatch, "/api/posts/:postId", updatePostHandler)
			rec := serveRoundTrip(e, roundTripToken, http.MethodPatch, "/api/posts/p1", `{"caption":"new caption"}`)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), tc.wantStatus)
			}
			queries := conn.recorded()
			// 以前はセッション確認 + isureal_update_post + getPostWithAuthor の 3 往復。
			if len(queries) != 1 {
				t.Fatalf("%d statements, want 1: %v", len(queries), queries)
			}
			if q := queries[0].query; !strings.Contains(q, "login_sessions") || !strings.Contains(q, "isureal_update_post_returning(s.sess_user_id, $1, $2, $3)") {
				t.Fatalf("query = %s", q)
			}
			if tc.wantStatus != http.StatusOK {
				return
			}
			var body struct {
				ID      string `json:"id"`
				Caption string `json:"caption"`
				Author  struct {
					ID string `json:"id"`
				} `json:"author"`
				ImageURL string `json:"imageUrl"`
				OnTime   bool   `json:"onTime"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.ID != "p1" || body.Caption != "new caption" || body.Author.ID != "u1" || !body.OnTime ||
				!strings.HasPrefix(body.ImageURL, "/api/images/i1?") {
				t.Fatalf("body = %s", rec.Body.String())
			}
		})
	}
}

func TestGetPostUsesOneStatementOnOneConnection(t *testing.T) {
	columns := append([]string{"sess_region", "found"}, updatePostReturningColumnNames[2:]...)
	row := func(sessRegion any, allowed bool) []driver.Value {
		values := updatePostReturningRow(sessRegion, nil, "hello")
		return append([]driver.Value{sessRegion, allowed}, values[2:]...)
	}
	cases := []struct {
		name       string
		rows       [][]driver.Value
		wantStatus int
	}{
		{"visible", [][]driver.Value{row("europe", true)}, http.StatusOK},
		{"not visible", [][]driver.Value{row("europe", false)}, http.StatusNotFound},
		{"region mismatch", [][]driver.Value{row("asia", true)}, http.StatusForbidden},
		{"invalid session", nil, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn := &recordingConnector{respond: func(string) ([]string, [][]driver.Value) {
				return columns, tc.rows
			}}
			e := roundTripServer(t, conn, http.MethodGet, "/api/posts/:postId", getPostHandler)
			rec := serveRoundTrip(e, roundTripToken, http.MethodGet, "/api/posts/p1", "")
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), tc.wantStatus)
			}
			queries := conn.recorded()
			// 以前の EU/US は投機的なセッション確認 + 投稿 + 閲覧者の文脈を別々の接続で並行に投げていた (3 文・接続 3 本)。
			if len(queries) != 1 {
				t.Fatalf("%d statements, want 1: %v", len(queries), queries)
			}
			if conn.maxInUse != 1 || conn.opened != 1 {
				t.Fatalf("connections: max in use %d, opened %d, want 1 and 1", conn.maxInUse, conn.opened)
			}
			q := queries[0].query
			if !strings.Contains(q, "login_sessions") || !strings.Contains(q, "isureal_post_access_read(s.sess_user_id, $1)") {
				t.Fatalf("query = %s", q)
			}
			if strings.Contains(q, "friendships") {
				t.Fatalf("viewer context (friends of friends) is still loaded: %s", q)
			}
		})
	}
}
