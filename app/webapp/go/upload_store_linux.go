package isureal

import (
	"bytes"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// Before Linux 5.8 syncfs did not report writeback errors.
var canSyncUploadBatch = sync.OnceValue(func() bool {
	var u unix.Utsname
	var major, minor int
	if unix.Uname(&u) != nil {
		return false
	}
	_, err := fmt.Sscanf(unix.ByteSliceToString(u.Release[:]), "%d.%d", &major, &minor)
	return err == nil && (major > 5 || major == 5 && minor >= 8)
})

type uploadWrite struct {
	path string
	data []byte
	tmp  string
	err  error
}

type uploadBatch struct {
	writes []uploadWrite
	done   chan struct{}
}

var uploadBatches struct {
	mu      sync.Mutex
	pending *uploadBatch
	disk    sync.Mutex
}

func writeUploadedImage(path string, data []byte) error {
	if !canSyncUploadBatch() || len(data) > 2<<20 {
		return writeImageFileDurable(path, data)
	}
	// Only collect arrivals while another group has the disk. An isolated
	// request takes the normal file/directory fsync path without a timer.
	q := &uploadBatches
	q.mu.Lock()
	if q.pending == nil && q.disk.TryLock() {
		q.mu.Unlock()
		defer q.disk.Unlock()
		return writeImageFileDurable(path, data)
	}
	b := q.pending
	if b == nil {
		b = &uploadBatch{done: make(chan struct{})}
		q.pending = b
	}
	i := len(b.writes)
	b.writes = append(b.writes, uploadWrite{path: path, data: data})
	if len(b.writes) == 32 {
		q.pending = nil
	}
	q.mu.Unlock()

	if i == 0 {
		q.disk.Lock()
		q.mu.Lock()
		if q.pending == b {
			q.pending = nil
		}
		q.mu.Unlock()
		if len(b.writes) == 1 {
			b.writes[0].err = writeImageFileDurable(path, data)
		} else {
			flushUploadBatch(b.writes, unix.Syncfs)
		}
		q.disk.Unlock()
		close(b.done)
	} else {
		<-b.done
	}
	return b.writes[i].err
}

// The first barrier makes complete temporary files durable before they can
// be read by name. The second makes every successful rename durable before
// its caller is allowed to commit the corresponding database row.
func flushUploadBatch(writes []uploadWrite, syncFS func(int) error) {
	// Open before dirtying anything so syncfs sees writeback errors from
	// the whole operation. Staging here also ensures that any successful
	// rename stays on the very filesystem we synced.
	root, err := os.Open(imageStoreDir)
	if err != nil {
		for i := range writes {
			writes[i].err = writeImageFileDurable(writes[i].path, writes[i].data)
		}
		return
	}
	defer root.Close()
	for i := range writes {
		w := &writes[i]
		w.tmp, w.err = stageImageFile(imageStoreDir, bytes.NewReader(w.data), false)
	}
	dataErr := syncFS(int(root.Fd()))
	renamed := false
	for i := range writes {
		w := &writes[i]
		if w.err != nil {
			continue
		}
		w.err = dataErr
		if w.err == nil {
			w.err = os.Rename(w.tmp, w.path)
		}
		if w.err != nil {
			os.Remove(w.tmp)
		} else {
			renamed = true
		}
	}
	if renamed {
		if err := syncFS(int(root.Fd())); err != nil {
			for i := range writes {
				if writes[i].err == nil {
					writes[i].err = err
				}
			}
		}
	}
}
