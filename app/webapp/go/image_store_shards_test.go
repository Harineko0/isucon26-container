package isureal

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/labstack/echo/v5"
)

func setupShardedStore(t *testing.T) {
	t.Helper()
	oldDir, oldDisk, oldStore, oldAccel := imageStoreDir, imageDiskDir, imageDataStore, imageAccelEnabled
	imageStoreDir = filepath.Join(t.TempDir(), "imgstore")
	imageDiskDir = filepath.Join(t.TempDir(), "imgcache")
	imageDataStore = newImageDataCache()
	imageAccelEnabled = true
	t.Cleanup(func() {
		imageStoreDir, imageDiskDir, imageDataStore, imageAccelEnabled = oldDir, oldDisk, oldStore, oldAccel
	})
	if err := ensureImageStoreDirs(); err != nil {
		t.Fatal(err)
	}
}

func TestTimeOrderedImagesUseDistributedStoreDirectories(t *testing.T) {
	setupShardedStore(t)
	dirs := map[string]int{}
	for range 4096 {
		id := newImageID("asia")
		path, ok := imageStorePath(id)
		if !ok || filepath.Base(path) != id || filepath.Dir(filepath.Dir(path)) != imageStoreDir {
			t.Fatalf("invalid store path %q for %q", path, id)
		}
		dirs[filepath.Dir(path)]++
	}
	if len(dirs) < 240 {
		t.Fatalf("time-ordered IDs concentrated in %d directories", len(dirs))
	}
	for _, bad := range []string{"", "../../secret", "00112233-4455-6677-8899-../../etc/passwd"} {
		if _, ok := imageStorePath(bad); ok {
			t.Fatalf("accepted unsafe id %q", bad)
		}
	}
}

func TestStoreReadAndAccelKeepLegacyFiles(t *testing.T) {
	setupShardedStore(t)
	id := "019ab12c-1234-4567-89ab-fe12345678f1"
	data := []byte("already persisted legacy bytes")
	legacy := filepath.Join(imageStoreDir, id[:2], id)
	if err := writeImageFileDurable(legacy, data); err != nil {
		t.Fatal(err)
	}
	for _, accel := range []bool{true, false} {
		imageAccelEnabled = accel
		cache := newImageDataCache()
		got, uri, ok := cache.locate(id)
		if !ok {
			t.Fatal("lost image created before sharding changed")
		}
		if accel && uri != "/_imgstore/01/"+id {
			t.Fatalf("legacy X-Accel-Redirect=%q", uri)
		}
		if !accel && !bytes.Equal(got, data) {
			t.Fatal("legacy image bytes changed")
		}
	}
	imageAccelEnabled = true
	// A partially written new path must not hide a durable old copy.
	path, _ := imageStorePath(id)
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, ok := readImageFromStore(id); !ok || !bytes.Equal(got, data) {
		t.Fatal("empty new path hid legacy file")
	}
}

func TestShardedUploadsRemainReadableWithoutMemoryCache(t *testing.T) {
	setupShardedStore(t)
	type upload struct {
		id   string
		data []byte
	}
	var uploads []upload
	for i := range 32 {
		uploads = append(uploads, upload{newImageID("europe"), bytes.Repeat([]byte{byte(i)}, 2048+i)})
	}
	var wg sync.WaitGroup
	for _, u := range uploads {
		wg.Go(func() {
			dbData, region := storeUploadedImage(u.id, u.data, "europe")
			if len(dbData) != 0 || region == nil || *region != "europe" {
				t.Errorf("failed to persist %s off DB", u.id)
			}
		})
	}
	wg.Wait()
	for _, u := range uploads {
		// Recreate the cache as after a restart. Data must come from disk.
		got, ok := newImageDataCache().get(u.id)
		if !ok || !bytes.Equal(got, u.data) {
			t.Fatalf("cold read lost durable upload %s", u.id)
		}
		_, uri, ok := newImageDataCache().locate(u.id)
		path, _ := imageStorePath(u.id)
		if !ok || filepath.Join(imageStoreDir, uri[len(imageStoreAccelPrefix):]) != path {
			t.Fatalf("nginx would read a different path: %q", uri)
		}
		region := "europe"
		discardUploadedImage(u.id, &region)
		if _, ok := readImageFromStore(u.id); ok {
			t.Fatal("discard left a new upload on disk")
		}
	}
}

func TestPeerImageTransferUsesShardedStoreAndReadsLegacy(t *testing.T) {
	setupShardedStore(t)
	oldSecret := internalSecret.Load()
	t.Cleanup(func() { internalSecret.Store(oldSecret) })
	setInternalSecret(&Config{DBPassword: "test-secret"})
	e := echo.New()
	e.Pre(InternalMiddleware(&HandlerContext{}))
	request := func(method, id string, data []byte) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/internal/images/"+id, bytes.NewReader(data))
		r.Header.Set(internalSecretHeader, *internalSecret.Load())
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		return w
	}
	for _, legacy := range []bool{false, true} {
		id := newImageID("america")
		data := []byte(fmt.Sprintf("peer bytes legacy=%t", legacy))
		if legacy {
			if err := writeImageFileDurable(filepath.Join(imageStoreDir, id[:2], id), data); err != nil {
				t.Fatal(err)
			}
		} else if w := request(http.MethodPut, id, data); w.Code != http.StatusNoContent {
			t.Fatalf("peer PUT: %d", w.Code)
		}
		imageDataStore = newImageDataCache()
		w := request(http.MethodGet, id, nil)
		if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), data) {
			t.Fatalf("peer cold GET: %d %q", w.Code, w.Body.Bytes())
		}
	}
}
