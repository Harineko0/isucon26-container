package isureal

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

// syncRecorder は fsync の呼び出しを記録する。ファイルの fsync はディレクトリの fsync より前に来る。
type syncRecorder struct {
	mu    sync.Mutex
	calls []string // "file:<path>" / "dir:<path>" の順
	block chan struct{}
	fail  map[string]error
}

func (r *syncRecorder) file(path string) error {
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err, ok := r.fail[path]; ok {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return err
	}
	r.calls = append(r.calls, "file:"+path)
	return nil
}

func (r *syncRecorder) dir(path string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "dir:"+path)
	return nil
}

func (r *syncRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.calls)
}

// installTestSyncer は imageSyncer を記録付きの新しい syncer に差し替える (run は起こさない)。
func installTestSyncer(t *testing.T) (*imageFileSyncer, *syncRecorder) {
	t.Helper()
	setupShardedStore(t)
	rec := &syncRecorder{fail: map[string]error{}}
	s := newImageFileSyncer()
	s.fsyncFile, s.fsyncDir = rec.file, rec.dir
	old := imageSyncer
	imageSyncer = s
	t.Cleanup(func() { imageSyncer = old })
	return s, rec
}

func TestWriteImageFileIsReadableBeforeFsync(t *testing.T) {
	s, rec := installTestSyncer(t)
	id := newImageID("asia")
	path, _ := imageStorePath(id)
	data := bytes.Repeat([]byte{7}, 4096)
	if err := writeImageFile(path, data); err != nil {
		t.Fatal(err)
	}
	// rename まで済んでいるので、fsync の前から読める (nginx の sendfile と同じくパスで見る)。
	if got, ok := readImageFromStore(id); !ok || !bytes.Equal(got, data) {
		t.Fatal("renamed image is not readable before fsync")
	}
	if _, uri, ok := newImageDataCache().locate(id); !ok || uri == "" {
		t.Fatalf("nginx redirect unavailable before fsync: %q %v", uri, ok)
	}
	if temps, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".tmp-*")); len(temps) != 0 {
		t.Fatalf("temporary file left behind: %v", temps)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Fatalf("permissions: %v %v", fi, err)
	}
	if calls := rec.snapshot(); len(calls) != 0 {
		t.Fatalf("fsync ran inside the request: %v", calls)
	}
	if s.pendingCount() != 1 {
		t.Fatalf("pending = %d, want 1", s.pendingCount())
	}
	s.flush()
	want := []string{"file:" + path, "dir:" + filepath.Dir(path)}
	if got := rec.snapshot(); !slices.Equal(got, want) {
		t.Fatalf("fsync calls = %v, want %v", got, want)
	}
	if s.pendingCount() != 0 || s.synced.Load() != 1 || s.failed.Load() != 0 {
		t.Fatalf("after flush: pending=%d synced=%d failed=%d", s.pendingCount(), s.synced.Load(), s.failed.Load())
	}
	// 2 回目の flush は何もしない。
	s.flush()
	if got := rec.snapshot(); len(got) != 2 {
		t.Fatalf("flush without pending files fsynced again: %v", got)
	}
}

func TestWriteImageFileDurableStillSyncsInline(t *testing.T) {
	s, rec := installTestSyncer(t)
	id := newImageID("asia")
	path, _ := imageStorePath(id)
	if err := writeImageFileDurable(path, []byte("legacy durable write")); err != nil {
		t.Fatal(err)
	}
	// 従来の経路は要求の中で fsync するので、syncer には積まない (テストや移行前のファイルの用意で使う)。
	if s.pendingCount() != 0 || len(rec.snapshot()) != 0 {
		t.Fatalf("durable write used the syncer: pending=%d calls=%v", s.pendingCount(), rec.snapshot())
	}
}

func TestImageSyncerFsyncsFilesThenEachDirectoryOnce(t *testing.T) {
	s, rec := installTestSyncer(t)
	// 同じシャードに 3 件、別のシャードに 1 件。
	var paths []string
	for _, shard := range []string{"ab", "ab", "ab", "cd"} {
		id := newImageID("europe")
		id = id[:24] + shard + id[26:]
		path, ok := imageStorePath(id)
		if !ok || filepath.Base(filepath.Dir(path)) != shard {
			t.Fatalf("unexpected shard for %s: %s", id, path)
		}
		if err := writeImageFile(path, []byte("x"+shard)); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	s.flush()
	calls := rec.snapshot()
	var files, dirs []string
	for _, c := range calls {
		if c[:5] == "file:" {
			if len(dirs) != 0 {
				t.Fatalf("file fsync after a directory fsync: %v", calls)
			}
			files = append(files, c[5:])
		} else {
			dirs = append(dirs, c[4:])
		}
	}
	if !slices.Equal(files, paths) {
		t.Fatalf("files fsynced = %v, want %v", files, paths)
	}
	slices.Sort(dirs)
	want := []string{filepath.Join(imageStoreDir, "ab"), filepath.Join(imageStoreDir, "cd")}
	if !slices.Equal(dirs, want) {
		t.Fatalf("directories fsynced = %v, want %v (each once)", dirs, want)
	}
	if s.synced.Load() != 4 {
		t.Fatalf("synced = %d, want 4", s.synced.Load())
	}
}

// DB に入らなかった画像 (ロールバック) や initialize で退避された画像は、fsync の前に消えていてよい。
func TestImageSyncerIgnoresFilesRemovedBeforeFsync(t *testing.T) {
	s, _ := installTestSyncer(t)
	s.fsyncFile, s.fsyncDir = fsyncPath, syncDir // 実際の fsync で ErrNotExist になることを見る
	id := newImageID("asia")
	dbData, region := storeUploadedImage(id, []byte("rolled back"), "asia")
	if len(dbData) != 0 || region == nil || *region != "asia" {
		t.Fatalf("storeUploadedImage = %q %v", dbData, region)
	}
	discardUploadedImage(id, region)
	kept := newImageID("asia")
	if _, region := storeUploadedImage(kept, []byte("kept"), "asia"); region == nil {
		t.Fatal("second upload fell back to the DB")
	}
	if err := resetImageStore(); err != nil { // initialize: ディレクトリごと退避
		t.Fatal(err)
	}
	s.flush()
	if s.failed.Load() != 0 || s.synced.Load() != 0 {
		t.Fatalf("removed files counted: failed=%d synced=%d", s.failed.Load(), s.synced.Load())
	}
	// 失敗は数える。
	s.fsyncFile = func(string) error { return errors.New("EIO") }
	path, _ := imageStorePath(newImageID("asia"))
	if err := writeImageFile(path, []byte("io error")); err != nil {
		t.Fatal(err)
	}
	s.flush()
	if s.failed.Load() != 1 {
		t.Fatalf("failed = %d, want 1", s.failed.Load())
	}
}

func TestImageSyncerRunWakesOnBatchAndInterval(t *testing.T) {
	s, rec := installTestSyncer(t)
	go s.run()
	// imageSyncBatch 件で、間隔を待たずに始まる。
	for range imageSyncBatch {
		path, _ := imageStorePath(newImageID("america"))
		if err := writeImageFile(path, []byte("batch")); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(imageSyncInterval / 2)
	for len(rec.snapshot()) < imageSyncBatch && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if n := len(rec.snapshot()); n < imageSyncBatch {
		t.Fatalf("batch of %d did not start fsync before the interval: %d calls", imageSyncBatch, n)
	}
	// 1 件だけなら間隔で始まる。
	before := len(rec.snapshot())
	path, _ := imageStorePath(newImageID("america"))
	if err := writeImageFile(path, []byte("single")); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(20 * imageSyncInterval)
	for len(rec.snapshot()) <= before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !slices.Contains(rec.snapshot(), "file:"+path) {
		t.Fatal("single file was not fsynced by the interval")
	}
}

// flush は進行中の 1 回分が終わるのも待ち、その間に積まれたものも含めてから返る (SIGTERM の経路)。
func TestImageSyncerFlushWaitsForInFlightBatch(t *testing.T) {
	s, rec := installTestSyncer(t)
	rec.block = make(chan struct{})
	first, _ := imageStorePath(newImageID("asia"))
	if err := writeImageFile(first, []byte("first")); err != nil {
		t.Fatal(err)
	}
	inflight := make(chan struct{})
	go func() {
		defer close(inflight)
		s.flush() // rec.block で止まる
	}()
	time.Sleep(10 * time.Millisecond)
	second, _ := imageStorePath(newImageID("asia"))
	if err := writeImageFile(second, []byte("second")); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.flush()
	}()
	select {
	case <-done:
		t.Fatal("flush returned while the first batch was still being fsynced")
	case <-time.After(20 * time.Millisecond):
	}
	close(rec.block)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("flush did not finish")
	}
	<-inflight
	calls := rec.snapshot()
	if !slices.Contains(calls, "file:"+first) || !slices.Contains(calls, "file:"+second) {
		t.Fatalf("flush skipped a file: %v", calls)
	}
}

// 1 件ごとに fsync (従来) と、rename まで (新) + まとめて fsync の、要求側の時間。
func BenchmarkUploadImageWrite(b *testing.B) {
	dir := b.TempDir()
	old := imageStoreDir
	imageStoreDir = filepath.Join(dir, "imgstore")
	b.Cleanup(func() { imageStoreDir = old })
	if err := ensureImageStoreDirs(); err != nil {
		b.Fatal(err)
	}
	data := bytes.Repeat([]byte{0xa5}, 355*1024)
	b.Run("durable-per-request", func(b *testing.B) {
		for range b.N {
			path, _ := imageStorePath(newImageID("asia"))
			if err := writeImageFileDurable(path, data); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("write-behind-request-side", func(b *testing.B) {
		s := newImageFileSyncer()
		oldSyncer := imageSyncer
		imageSyncer = s
		b.Cleanup(func() { imageSyncer = oldSyncer })
		for range b.N {
			path, _ := imageStorePath(newImageID("asia"))
			if err := writeImageFile(path, data); err != nil {
				b.Fatal(err)
			}
		}
		b.StopTimer()
		s.flush()
	})
}
