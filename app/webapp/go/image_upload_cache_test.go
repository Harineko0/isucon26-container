package isureal

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUploadedImageCacheUsesDurableFile(t *testing.T) {
	oldStore, oldDisk, oldAccel := imageStoreDir, imageDiskDir, imageAccelEnabled
	t.Cleanup(func() { imageStoreDir, imageDiskDir, imageAccelEnabled = oldStore, oldDisk, oldAccel })
	imageStoreDir, imageDiskDir, imageAccelEnabled = t.TempDir(), t.TempDir(), true

	id := newImageID("europe")
	body := []byte("uploaded image")
	dbData, region := storeUploadedImage(id, body, "europe")
	if region == nil || len(dbData) != 0 {
		t.Fatal("upload did not reach the durable store")
	}
	cache := newImageDataCache()
	cache.putUpload(id, body, region)
	data, uri, ok := cache.locate(id)
	if !ok || data != nil || !strings.HasPrefix(uri, imageStoreAccelPrefix) || cache.currentBytes != 0 {
		t.Fatalf("expected only a file redirect, got uri %q, hit %v, heap bytes %d", uri, ok, cache.currentBytes)
	}
	file, err := os.ReadFile(filepath.Join(imageStoreDir, strings.TrimPrefix(uri, imageStoreAccelPrefix)))
	if err != nil || !bytes.Equal(file, body) {
		t.Fatalf("redirected file did not match the upload, err %v", err)
	}

	// DB にフォールバックした画像、または nginx を使わない構成は従来どおり本文を返す。
	dbID := newImageID("europe")
	cache.putUpload(dbID, body, nil)
	data, uri, ok = cache.locate(dbID)
	if !ok || uri != "" || !bytes.Equal(data, body) {
		t.Fatal("DB-backed upload not served from memory")
	}
	imageAccelEnabled = false
	cache.putUpload(id, body, region)
	if !bytes.Equal(cache.current[id], body) {
		t.Fatal("directly served upload not retained")
	}
}
