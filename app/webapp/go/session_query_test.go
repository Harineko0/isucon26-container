package isureal

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
)

func newSessionTestContext(tb testing.TB, deferredToken string) *echo.Context {
	tb.Helper()
	c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/api/me", nil), httptest.NewRecorder())
	c.Set("session", &Session{UserID: "u1", Region: "asia"})
	if deferredToken != "" {
		c.Set("deferred_session", deferredToken)
	}
	return c
}

func apiStatus(tb testing.TB, err error) int {
	tb.Helper()
	if err == nil {
		return 0
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		tb.Fatalf("not an APIError: %v", err)
	}
	return apiErr.Status
}

func TestFinishWithSession(t *testing.T) {
	europe := "europe"
	asia := "asia"
	tests := map[string]struct {
		deferred   bool
		found      bool
		sessRegion *string
		wantStatus int // 0 = nil error
		wantCached bool
	}{
		"verified by middleware: nothing to check":   {deferred: false, found: true, sessRegion: nil, wantStatus: 0, wantCached: true},
		"deferred, session row present, same region": {deferred: true, found: true, sessRegion: &asia, wantStatus: 0, wantCached: true},
		"deferred, no session row: 401 and forget":   {deferred: true, found: false, sessRegion: nil, wantStatus: http.StatusUnauthorized, wantCached: false},
		"deferred, other region: 403 and forget":     {deferred: true, found: true, sessRegion: &europe, wantStatus: http.StatusForbidden, wantCached: false},
		"deferred, region missing: 403":              {deferred: true, found: true, sessRegion: nil, wantStatus: http.StatusForbidden, wantCached: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			token := "token-" + strings.ReplaceAll(name, " ", "-")
			knownSessions.Store(token, Session{UserID: "u1", Region: "asia"})
			defer knownSessions.Delete(token)
			h := &HandlerContext{Config: &Config{Region: "asia"}}
			c := newSessionTestContext(t, "")
			if tt.deferred {
				c.Set("deferred_session", token)
			}
			err := finishWithSession(c, h, token, tt.deferred, tt.found, tt.sessRegion)
			if got := apiStatus(t, err); got != tt.wantStatus {
				t.Fatalf("status = %d, want %d (err %v)", got, tt.wantStatus, err)
			}
			if _, cached := knownSessions.Load(token); cached != tt.wantCached {
				t.Fatalf("cached = %v, want %v", cached, tt.wantCached)
			}
			if tt.wantStatus == http.StatusUnauthorized && GetSession(c) != nil {
				t.Fatalf("session should be cleared on 401")
			}
		})
	}
}

func TestSessionSource(t *testing.T) {
	t.Run("verified session: user id appended as the next parameter", func(t *testing.T) {
		c := newSessionTestContext(t, "")
		source, args, tokenHash, deferred := sessionSource(c, []any{"p1", "p2"})
		if deferred || tokenHash != "" {
			t.Fatalf("deferred = %v, tokenHash = %q", deferred, tokenHash)
		}
		if len(args) != 3 || args[2] != "u1" {
			t.Fatalf("args = %v", args)
		}
		if !strings.Contains(source, "$3::text AS sess_user_id") || !strings.Contains(source, "NULL::text AS sess_region") {
			t.Fatalf("source = %s", source)
		}
	})
	t.Run("deferred session: token hash appended and login_sessions joined", func(t *testing.T) {
		c := newSessionTestContext(t, "abc")
		source, args, tokenHash, deferred := sessionSource(c, nil)
		if !deferred || tokenHash != "abc" {
			t.Fatalf("deferred = %v, tokenHash = %q", deferred, tokenHash)
		}
		if len(args) != 1 || args[0] != "abc" {
			t.Fatalf("args = %v", args)
		}
		if !strings.Contains(source, "s.token_hash = $1") || !strings.Contains(source, "u.status = 'active'") {
			t.Fatalf("source = %s", source)
		}
	})
}

func TestUserColumnsCoalesced(t *testing.T) {
	got := userColumnsCoalesced("r", "author")
	for _, want := range []string{
		`COALESCE(r."author.id", '') AS "author.id"`,
		`COALESCE(r."author.secret_key_hash", '') AS "author.secret_key_hash"`,
		`r."author.profile_image_id" AS "author.profile_image_id"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, `COALESCE(r."author.profile_image_id"`) {
		t.Fatalf("profile_image_id must keep NULL: %s", got)
	}
	plain := userColumnsCoalesced("r", "")
	if !strings.Contains(plain, `COALESCE(r.id, '') AS id`) || !strings.Contains(plain, `r.profile_image_id AS profile_image_id`) {
		t.Fatalf("plain = %s", plain)
	}
	if n := strings.Count(plain, " AS "); n != len(userColumns) {
		t.Fatalf("%d columns, want %d", n, len(userColumns))
	}
}

func TestPostWithAuthorColumnsCoalesced(t *testing.T) {
	got := postWithAuthorColumnsCoalesced("r")
	for _, want := range []string{
		`COALESCE(r.id, '') AS id`,
		`COALESCE(r.posted_at, 'epoch') AS posted_at`,
		`COALESCE(r.on_time_deadline, 'epoch') AS on_time_deadline`,
		`r.window_ended_at`,
		`COALESCE(r."author.id", '') AS "author.id"`,
		`r."author.profile_image_id" AS "author.profile_image_id"`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %s", want, got)
		}
	}
}

func TestFeedDeferredQueriesAndRoutes(t *testing.T) {
	for _, route := range []string{
		"GET /api/posts/me/current",
		"GET /api/posts/:postId",
		"PATCH /api/posts/:postId",
		"DELETE /api/posts/:postId",
		"GET /api/feeds/:feed",
		"GET /api/me/export",
		"GET /api/users",
		"DELETE /api/comments/:commentId",
		"DELETE /api/realmojis/:realMojiId",
	} {
		if !deferredSessionRoutes[route] {
			t.Fatalf("route %s should be in deferredSessionRoutes", route)
		}
	}
	for _, feed := range []string{"public", "following", "friends"} {
		q := feedDeferredQueries[feed]
		if !strings.Contains(q, "u.region = $4") || !strings.Contains(q, "s.token_hash = $5") {
			t.Fatalf("feedDeferredQueries[%s] missing session parameters: %s", feed, q)
		}
	}
	fof := feedDeferredQueries["friends-of-friends"]
	if !strings.Contains(fof, "u.region = $5") || !strings.Contains(fof, "s.token_hash = $6") {
		t.Fatalf("feedDeferredQueries[friends-of-friends] missing session parameters: %s", fof)
	}
}
