package isureal

import (
	"bytes"
	"expvar"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"example.com/isureal-company/isureal/internal/jpegcheck"
	"github.com/labstack/echo/v5"
)

func TestCachedJPEGKeepsProfileSizeAndAbuseChecks(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 513, 1)), nil); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	// This is valid for a post, so it can already be in the shared cache.
	if _, _, err := jpegcheck.ValidateBytes(data); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		data  []byte
		abuse string
	}{
		{data, ""},
		{data[:len(data)/2], "invalid_image"},
	} {
		e := echo.New()
		req := httptest.NewRequest(http.MethodPut, "/api/me/profile-image", bytes.NewReader(tc.data))
		req.Header.Set("Content-Type", "image/jpeg")
		c := e.NewContext(req, httptest.NewRecorder())
		err := putProfileImageHandler(&HandlerContext{})(c)
		apiErr, ok := err.(*APIError)
		if !ok || apiErr.Status != 400 || apiErr.Code != "invalid_profile_image" {
			t.Fatalf("oversized/corrupt image accepted: %v", err)
		}
		abuse, _ := c.Get("abuse").(string)
		if abuse != tc.abuse {
			t.Fatalf("abuse=%q, want %q", abuse, tc.abuse)
		}
	}
}

func TestPeerImageResetClearsJPEGValidationCache(t *testing.T) {
	oldDir := imageStoreDir
	imageStoreDir = filepath.Join(t.TempDir(), "store")
	t.Cleanup(func() { imageStoreDir = oldDir })
	setInternalSecret(&Config{DBPassword: "test"})
	var buf bytes.Buffer
	jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 1, 1)), nil)
	jpegcheck.ValidateBytes(buf.Bytes())
	jpegcheck.ValidateBytes(buf.Bytes())
	e := echo.New()
	e.Pre(InternalMiddleware(&HandlerContext{}))
	req := httptest.NewRequest(http.MethodPost, "/api/internal/images/reset", nil)
	req.Header.Set(internalSecretHeader, *internalSecret.Load())
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("reset: %d %s", rec.Code, rec.Body.String())
	}
	if stats := expvar.Get("jpeg_validation_cache").String(); stats != `{"hits":0,"misses":0}` {
		t.Fatalf("peer reset left cached validations: %s", stats)
	}
}

func TestInitializeRequiresAllPeerResetAcknowledgements(t *testing.T) {
	oldPeers := regionPeerHosts
	t.Cleanup(func() { regionPeerHosts = oldPeers })
	var requests atomic.Int32
	var fail atomic.Bool
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/internal/images/reset" || !validInternalSecret(r) {
			t.Errorf("unexpected reset request: %s %s", r.Method, r.URL)
		}
		requests.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer peer.Close()
	setInternalSecret(&Config{DBPassword: "test"})
	host := strings.TrimPrefix(peer.URL, "http://")
	regionPeerHosts = map[string]string{"asia": "unused", "europe": host, "america": host}
	if err := resetPeerImageStores("asia"); err != nil || requests.Load() != 2 {
		t.Fatalf("did not reset both peers: %v, requests=%d", err, requests.Load())
	}
	fail.Store(true)
	if err := resetPeerImageStores("asia"); err == nil {
		t.Fatal("initialize must fail if any peer reset fails")
	}
}
