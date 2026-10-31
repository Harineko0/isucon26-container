package isureal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// deferred 経路 (knownSessions にトークンがあり、セッションの確認をハンドラの SQL に同居させる) でも、
// 参照実装と同じく 401 (無効なセッション) → 403 (リージョン違い) → ハンドラのエラー (400 など) の順になること。
func TestDeferredSessionRejectsBeforeHandlerError(t *testing.T) {
	const token = "deferred-session-test-token"
	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])
	h := &HandlerContext{Config: &Config{Region: "asia"}}

	badRequest := func(c *echo.Context) error {
		return NewAPIError(http.StatusBadRequest, "invalid_interaction")
	}
	// callWithSession が 1 文でセッションを確認し、その後にハンドラがエラーを返した場合の模擬。
	checkedThenNotFound := func(c *echo.Context) error {
		c.Set(deferredSessionCheckedKey, true)
		return NewAPIError(http.StatusNotFound, "post_not_found")
	}
	created := func(c *echo.Context) error {
		return c.NoContent(http.StatusCreated)
	}

	cases := []struct {
		name        string
		lookup      *Session
		handler     echo.HandlerFunc
		wantStatus  int
		wantLookups int
	}{
		{"invalid session beats 400", nil, badRequest, http.StatusUnauthorized, 1},
		{"region mismatch beats 400", &Session{UserID: "u1", Region: "europe"}, badRequest, http.StatusForbidden, 1},
		{"valid session returns the 400", &Session{UserID: "u1", Region: "asia"}, badRequest, http.StatusBadRequest, 1},
		{"handler already checked the session", nil, checkedThenNotFound, http.StatusNotFound, 0},
		{"success path does not look up", nil, created, http.StatusCreated, 0},
	}
	defer func() { lookupSessionFunc = lookupSession }()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			knownSessions.Store(tokenHash, Session{UserID: "u1", TokenHash: tokenHash, Region: "asia"})
			lookups := 0
			lookupSessionFunc = func(ctx context.Context, db *sqlx.DB, hash string) (*Session, error) {
				lookups++
				if hash != tokenHash {
					t.Fatalf("looked up %q, want %q", hash, tokenHash)
				}
				return tc.lookup, nil
			}
			e := echo.New()
			e.HTTPErrorHandler = APIErrorHandler
			e.POST("/api/posts/:postId/comments", tc.handler, SessionMiddleware(h, true), RegionMiddleware(h))
			req := httptest.NewRequest(http.MethodPost, "/api/posts/p1/comments", nil)
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), tc.wantStatus)
			}
			if lookups != tc.wantLookups {
				t.Fatalf("session lookups = %d, want %d", lookups, tc.wantLookups)
			}
			if tc.wantStatus == http.StatusUnauthorized || tc.wantStatus == http.StatusForbidden {
				if _, still := knownSessions.Load(tokenHash); still {
					t.Fatalf("rejected session should be forgotten")
				}
			}
		})
	}
}
