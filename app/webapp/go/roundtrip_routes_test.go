package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// EU/US は DB まで往復 ~100ms。ログイン済みの利用者のリクエストは、セッションの確認をハンドラの
// 1 文に同居させて 1 往復で済ませている (#17, #82)。このテストは registerRoutes の全ルートを本番と
// 同じミドルウェアで通し、1 リクエストあたりの文の数と同時に握る接続の数を数える。
// 文を足したり、ルートを deferredSessionRoutes から外したりして往復が増えたら落ちる。

const (
	rtPostID   = "01900000-0000-7000-8000-000000000001"
	rtUserID   = "01900000-0000-7000-8000-000000000002"
	rtOtherID  = "01900000-0000-7000-8000-000000000003"
	rtCommentR = "01900000-0000-7000-8000-000000000004"
)

type routeRequest struct {
	method, path string // registerRoutes に登録したパス
	target, body string
}

// sessionRouteRequests は、ログイン済みの利用者が叩くルートと、入力検証を通るリクエスト。
var sessionRouteRequests = []routeRequest{
	{http.MethodPost, "/api/auth/logout", "/api/auth/logout", ""},
	{http.MethodGet, "/api/users", "/api/users?q=alice", ""},
	{http.MethodGet, "/api/users/:userId", "/api/users/" + rtOtherID, ""},
	{http.MethodGet, "/api/me/export", "/api/me/export", ""},
	{http.MethodGet, "/api/me", "/api/me", ""},
	{http.MethodPatch, "/api/me", "/api/me", `{"bio":"hello"}`},
	{http.MethodDelete, "/api/me/profile-image", "/api/me/profile-image", ""},
	{http.MethodPost, "/api/me/account-deletion", "/api/me/account-deletion", ""},
	{http.MethodGet, "/api/posting-window/current", "/api/posting-window/current", ""},
	{http.MethodGet, "/api/posts/me/current", "/api/posts/me/current", ""},
	{http.MethodGet, "/api/posts/:postId", "/api/posts/" + rtPostID, ""},
	{http.MethodPatch, "/api/posts/:postId", "/api/posts/" + rtPostID, `{"caption":"new caption"}`},
	{http.MethodDelete, "/api/posts/:postId", "/api/posts/" + rtPostID, ""},
	{http.MethodGet, "/api/feeds/:feed", "/api/feeds/friends", ""},
	{http.MethodGet, "/api/memories", "/api/memories?month=2026-10", ""},
	{http.MethodGet, "/api/memories/:date", "/api/memories/2026-10-31", ""},
	{http.MethodPost, "/api/posts/:postId/comments", "/api/posts/" + rtPostID + "/comments", `{"body":"nice"}`},
	{http.MethodGet, "/api/posts/:postId/comments", "/api/posts/" + rtPostID + "/comments", ""},
	{http.MethodDelete, "/api/comments/:commentId", "/api/comments/" + rtCommentR, ""},
	{http.MethodGet, "/api/posts/:postId/realmojis", "/api/posts/" + rtPostID + "/realmojis", ""},
	{http.MethodDelete, "/api/realmojis/:realMojiId", "/api/realmojis/" + rtCommentR, ""},
	{http.MethodPost, "/api/friend-requests", "/api/friend-requests", `{"recipientId":"` + rtOtherID + `"}`},
	{http.MethodPost, "/api/friend-requests/:requestId/:action", "/api/friend-requests/" + rtCommentR + "/accept", ""},
	{http.MethodGet, "/api/relationships/:kind", "/api/relationships/incoming-requests", ""},
	{http.MethodDelete, "/api/friendships/:userId", "/api/friendships/" + rtOtherID, ""},
	{http.MethodPut, "/api/follows/:userId", "/api/follows/" + rtOtherID, ""},
	{http.MethodDelete, "/api/follows/:userId", "/api/follows/" + rtOtherID, ""},
	{http.MethodPut, "/api/blocks/:userId", "/api/blocks/" + rtOtherID, ""},
	{http.MethodDelete, "/api/blocks/:userId", "/api/blocks/" + rtOtherID, ""},
}

// notCountedRoutes はこのテストで数えないルートと理由。
var notCountedRoutes = map[string]string{
	"POST /api/initialize":              "DB を作り直す",
	"POST /api/auth/register":           "セッションを使わない",
	"POST /api/auth/login":              "セッションを使わない (login_pipeline_test.go)",
	"GET /api/images/:imageId":          "セッションを使わない (image_read_batch_test.go)",
	"POST /api/posts":                   "multipart の画像検証とディスク書き込みが先に要る",
	"POST /api/posts/:postId/realmojis": "multipart の画像検証とディスク書き込みが先に要る",
	"PUT /api/me/profile-image":         "multipart の画像検証とディスク書き込みが先に要る",
	"GET /api/notifications":            "SSE の長い接続。投稿期間の watcher が要る (notification_api_test.go)",
}

func routeKey(method, path string) string { return method + " " + path }

// routesServer は registerRoutes の全ルートを、europe の利用者 u1 のセッションを覚えた状態で組む。
func routesServer(t *testing.T, conn *recordingConnector) *echo.Echo {
	t.Helper()
	db := sqlx.NewDb(sql.OpenDB(conn), "pgx")
	t.Cleanup(func() { db.Close() })
	h := &HandlerContext{DB: db, Config: &Config{Region: "europe"}}
	hash := roundTripTokenHash()
	knownSessions.Store(hash, Session{UserID: rtUserID, TokenHash: hash, Region: "europe"})
	t.Cleanup(func() { knownSessions.Delete(hash) })
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	registerRoutes(e, h)
	return e
}

// stubLookupSession は SessionMiddleware の別往復のセッション確認 (verifyDeferredSession) を数える。
func stubLookupSession(t *testing.T) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	orig := lookupSessionFunc
	lookupSessionFunc = func(context.Context, *sqlx.DB, string) (*Session, error) {
		calls.Add(1)
		return nil, nil
	}
	t.Cleanup(func() { lookupSessionFunc = orig })
	return &calls
}

// 新しいルートを足したら、sessionRouteRequests か notCountedRoutes のどちらかに入れる。
func TestEveryRouteIsCountedForRoundTrips(t *testing.T) {
	e := routesServer(t, &recordingConnector{})
	listed := map[string]bool{}
	for _, r := range sessionRouteRequests {
		listed[routeKey(r.method, r.path)] = true
	}
	var missing []string
	for _, r := range e.Router().Routes() {
		key := routeKey(r.Method, r.Path)
		if strings.HasPrefix(r.Path, "/api/admin/") || listed[key] || notCountedRoutes[key] != "" {
			continue
		}
		missing = append(missing, key)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("routes not in sessionRouteRequests or notCountedRoutes: %v", missing)
	}
}

// ログイン済みのルートはすべて deferred にして、ミドルウェアで別に lookupSession しない。
// (deferred でないと、書き込みは lookupSession + ハンドラの 2 往復、GET は投機的な確認で接続 2 本。)
func TestSessionRoutesAreDeferred(t *testing.T) {
	for _, r := range sessionRouteRequests {
		if !deferredSessionRoutes[routeKey(r.method, r.path)] {
			t.Errorf("%s is not in deferredSessionRoutes", routeKey(r.method, r.path))
		}
	}
}

// 無効になったセッション: ハンドラの最初の 1 文でセッションを確認し (0 行 → 401)、それより前にも後にも
// 文を投げない。入力検証で落ちて verifyDeferredSession の別往復に回ったらリクエストの組み方の誤り。
func TestInvalidSessionCostsOneStatement(t *testing.T) {
	for _, r := range sessionRouteRequests {
		t.Run(routeKey(r.method, r.path), func(t *testing.T) {
			lookups := stubLookupSession(t)
			conn := &recordingConnector{respond: func(string) ([]string, [][]driver.Value) {
				return []string{"sess_region"}, nil
			}}
			rec := serveRoundTrip(routesServer(t, conn), roundTripToken, r.method, r.target, r.body)
			if n := lookups.Load(); n != 0 {
				t.Fatalf("handler failed before its SQL (status %d %s) and the middleware looked up the session separately %d time(s)",
					rec.Code, rec.Body.String(), n)
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d (%s), want 401", rec.Code, rec.Body.String())
			}
			queries := conn.recorded()
			if len(queries) != 1 {
				t.Fatalf("%d statements, want 1: %v", len(queries), queries)
			}
			if !strings.Contains(queries[0].query, "login_sessions") {
				t.Fatalf("the statement does not check the session: %s", queries[0].query)
			}
			if conn.maxInUse != 1 {
				t.Fatalf("max connections in use = %d, want 1", conn.maxInUse)
			}
		})
	}
}
