package isureal

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func TestUploadBatchDurability(t *testing.T) {
	for _, failAt := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("failed_barrier_%d", failAt), func(t *testing.T) {
			setupShardedStore(t)
			path, _ := imageStorePath(newImageID("asia"))
			other, _ := imageStorePath(newImageID("europe"))
			writes := []uploadWrite{
				{path: path, data: []byte("first JPEG")},
				{path: filepath.Join(imageStoreDir, "missing", "id"), data: []byte("bad destination")},
				{path: other, data: []byte("another JPEG")},
				{path: other + "-empty"},
			}
			n := 0
			flushUploadBatch(writes, func(fd int) error {
				n++
				for _, i := range []int{0, 2} {
					w := writes[i]
					if n == 1 {
						if _, err := os.Stat(w.path); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("file visible before data sync: %v", err)
						}
					} else if data, err := os.ReadFile(w.path); err != nil || !bytes.Equal(data, w.data) {
						t.Fatalf("renamed file %d: %q, %v", i, data, err)
					}
				}
				if n == failAt {
					return unix.EIO
				}
				return unix.Syncfs(fd)
			})
			if n != 2 && !(failAt == 1 && n == 1) {
				t.Fatalf("got %d barriers", n)
			}
			for _, i := range []int{0, 2} {
				err := writes[i].err
				if (failAt == 0 && err != nil) || (failAt != 0 && !errors.Is(err, unix.EIO)) {
					t.Fatalf("image %d: got %v (failed barrier %d)", i, err, failAt)
				}
			}
			if writes[1].err == nil || !errors.Is(writes[3].err, errInvalidImageBody) {
				t.Fatalf("failed files reported as stored: %v / %v", writes[1].err, writes[3].err)
			}
			if temps, err := filepath.Glob(filepath.Join(imageStoreDir, ".tmp-*")); err != nil || len(temps) > 0 {
				t.Fatalf("staging files after batch: %v, %v", temps, err)
			}
		})
	}
}

// Run on a disk-backed Linux filesystem, e.g. -bench=BenchmarkUploadStore
// -benchtime=256x -cpu=2. Both paths produce the same files with the same
// durability contract, and include the wait for physical storage.
func BenchmarkUploadStore(b *testing.B) {
	data := make([]byte, 355075)
	rand.NewChaCha8([32]byte{1}).Read(data)
	for _, workers := range []int{1, 32, 128} {
		for _, method := range []struct {
			name  string
			write func(string, []byte) error
		}{{"individual", writeImageFileDurable}, {"group", writeUploadedImage}} {
			b.Run(fmt.Sprintf("%d/%s", workers, method.name), func(b *testing.B) {
				old := imageStoreDir
				imageStoreDir = filepath.Join(b.TempDir(), "imgstore")
				b.Cleanup(func() { imageStoreDir = old })
				if err := ensureImageStoreDirs(); err != nil {
					b.Fatal(err)
				}
				b.SetBytes(int64(len(data)))
				b.ResetTimer()
				var wg sync.WaitGroup
				for worker := range workers {
					wg.Go(func() {
						for i := worker; i < b.N; i += workers {
							path, _ := imageStorePath(newImageID("asia"))
							if err := method.write(path, data); err != nil {
								b.Error(err)
								return
							}
						}
					})
				}
				wg.Wait()
			})
		}
	}
}
