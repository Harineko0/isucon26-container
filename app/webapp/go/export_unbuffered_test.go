package isureal

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v5"
)

// #4: エクスポートの ZIP は利用者の全画像を含むので、SessionMiddleware の投機的な経路
// (bufferedResponseWriter) を通すと ZIP 全体がメモリに載る。どのリージョンでも
// GET /api/me/export は deferred 経路で処理され、ハンドラが書いたバイト列がその場で
// クライアント側の writer に届くこと。
func TestExportStreamsWithoutSessionBuffering(t *testing.T) {
	const token = "export-unbuffered-test-token"
	sum := sha256.Sum256([]byte(token))
	tokenHash := hex.EncodeToString(sum[:])
	posts, images, author := exportFixture(5)

	for _, region := range []string{"asia", "europe", "america"} {
		t.Run(region, func(t *testing.T) {
			if !deferredSessionRoutes["GET /api/me/export"] {
				t.Fatalf("GET /api/me/export must be a deferred session route")
			}
			knownSessions.Store(tokenHash, Session{UserID: author.ID, TokenHash: tokenHash, Region: region})
			defer knownSessions.Delete(tokenHash)

			h := &HandlerContext{Config: &Config{Region: region}}
			rec := httptest.NewRecorder()
			streamedDuringHandler := -1
			handler := func(c *echo.Context) error {
				// 本物のハンドラは selectWithSession の 1 文でセッションを確認する。
				c.Set(deferredSessionCheckedKey, true)
				if err := streamingExportResponse(c, posts, images, author); err != nil {
					return err
				}
				streamedDuringHandler = rec.Body.Len()
				return nil
			}
			e := echo.New()
			e.HTTPErrorHandler = APIErrorHandler
			e.GET("/api/me/export", handler, SessionMiddleware(h, true), RegionMiddleware(h))
			req := httptest.NewRequest(http.MethodGet, "/api/me/export", nil)
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
			e.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (%s), want 200", rec.Code, rec.Body.String())
			}
			if streamedDuringHandler != rec.Body.Len() || streamedDuringHandler == 0 {
				t.Fatalf("bytes reached the client while the handler ran = %d, final body = %d; the ZIP was buffered",
					streamedDuringHandler, rec.Body.Len())
			}
			zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
			if err != nil {
				t.Fatalf("response is not a valid ZIP: %v", err)
			}
			if got, want := len(zr.File), len(posts)+1; got != want {
				t.Fatalf("ZIP entries = %d, want %d", got, want)
			}
		})
	}
}
