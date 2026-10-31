package isureal

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// countImageStats records every path imageFileExists stats.
func countImageStats(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var paths []string
	orig := statImageFile
	statImageFile = func(path string) (os.FileInfo, error) {
		mu.Lock()
		paths = append(paths, path)
		mu.Unlock()
		return orig(path)
	}
	t.Cleanup(func() { statImageFile = orig })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), paths...)
	}
}

func writeTestImage(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("jpeg bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setupLocateDirs(t *testing.T) {
	t.Helper()
	oldDir, oldDisk, oldAccel := imageStoreDir, imageDiskDir, imageAccelEnabled
	imageStoreDir = filepath.Join(t.TempDir(), "imgstore")
	imageDiskDir = filepath.Join(t.TempDir(), "imgcache")
	imageAccelEnabled = true
	t.Cleanup(func() { imageStoreDir, imageDiskDir, imageAccelEnabled = oldDir, oldDisk, oldAccel })
}

// An upload (region-tagged id) in imgstore is found without a lookup in imgcache,
// the directory with the 1.8M initial images that can never hold it.
func TestLocateUploadSkipsInitialImageCache(t *testing.T) {
	setupLocateDirs(t)
	id := newImageID("asia")
	path, _ := imageStorePath(id)
	writeTestImage(t, path)
	stats := countImageStats(t)

	_, uri, ok := newImageDataCache().locate(id)
	if !ok || uri != imageStoreAccelPrefix+filepath.Base(filepath.Dir(path))+"/"+id {
		t.Fatalf("locate = %q, %v", uri, ok)
	}
	got := stats()
	for _, p := range got {
		if strings.HasPrefix(p, imageDiskDir) {
			t.Fatalf("looked up the upload in imgcache: %v", got)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%d stats, want 1: %v", len(got), got)
	}
}

// Initial images keep their single imgcache lookup, and an initial image whose random id
// happens to end in a region tag is still served from imgcache.
func TestLocateInitialImagesStillUseCache(t *testing.T) {
	setupLocateDirs(t)
	for _, id := range []string{
		"3c2a9d1e-1234-4567-89ab-0123456789ab", // untagged
		"3c2a9d1e-1234-4567-89ab-0123456789f2", // ends like a europe upload
	} {
		path, _ := imageDiskPath(id)
		writeTestImage(t, path)
		stats := countImageStats(t)
		_, uri, ok := newImageDataCache().locate(id)
		if !ok || uri != imageDiskAccelPrefix+id[:2]+"/"+id {
			t.Fatalf("%s: locate = %q, %v", id, uri, ok)
		}
		if _, tagged := imageRegionHint(id); !tagged {
			if n := len(stats()); n != 1 {
				t.Fatalf("%s: %d stats, want 1: %v", id, n, stats())
			}
		}
	}
}

// Uploads persisted under the old id[:2] layout are still found, and a missing image is a miss.
func TestLocateTaggedLegacyAndMissing(t *testing.T) {
	setupLocateDirs(t)
	id := "019ab12c-1234-4567-89ab-fe12345678f1"
	legacy := filepath.Join(imageStoreDir, id[:2], id)
	writeTestImage(t, legacy)
	if _, uri, ok := newImageDataCache().locate(id); !ok || uri != imageStoreAccelPrefix+id[:2]+"/"+id {
		t.Fatalf("legacy locate = %q, %v", uri, ok)
	}
	if _, _, ok := newImageDataCache().locate(newImageID("america")); ok {
		t.Fatal("found an image that is nowhere on disk")
	}
}
