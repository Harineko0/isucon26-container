package isureal

import (
	"os"
	"path/filepath"
	"regexp"
	"sync"
)

// 初期データの画像は cmd/imgsync でローカルディスクに書き出してある (不変なので内容は DB と同じ)。
const imageDiskDir = "/home/isucon/imgcache"

var imageIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func imageDiskPath(id string) (string, bool) {
	if !imageIDPattern.MatchString(id) {
		return "", false
	}
	return filepath.Join(imageDiskDir, id[:2], id), true
}

// WriteImageToDisk は画像を一時ファイル経由で書き出す (途中で止まっても壊れたファイルを残さない)。
func WriteImageToDisk(id string, data []byte) error {
	path, ok := imageDiskPath(id)
	if !ok {
		return nil
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readImageFromDisk(id string) ([]byte, bool) {
	path, ok := imageDiskPath(id)
	if !ok {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	return data, true
}

// imageCreatedAt は画像 id → created_at (不変)。パーティションを絞るために使う。
var imageCreatedAt sync.Map

// 画像の本体は id ごとに不変なので、リージョンごとにメモリへ置いて DB からの転送を省く。
// 閲覧可否は毎回 DB で確認するので、ここに残った削除済み画像が返ることはない。
const imageDataGenerationBytes = 384 << 20

type imageDataCache struct {
	mu           sync.RWMutex
	current      map[string][]byte
	previous     map[string][]byte
	currentBytes int
}

var imageDataStore = newImageDataCache()

func newImageDataCache() *imageDataCache {
	return &imageDataCache{current: map[string][]byte{}, previous: map[string][]byte{}}
}

func (c *imageDataCache) get(id string) ([]byte, bool) {
	c.mu.RLock()
	data, ok := c.current[id]
	if !ok {
		data, ok = c.previous[id]
	}
	c.mu.RUnlock()
	if ok && data != nil {
		return data, true
	}
	if data, ok := readImageFromDisk(id); ok {
		c.put(id, data)
		return data, true
	}
	return nil, false
}

func (c *imageDataCache) put(id string, data []byte) {
	if data == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.current[id]; ok {
		return
	}
	if c.currentBytes+len(data) > imageDataGenerationBytes {
		c.previous = c.current
		c.current = make(map[string][]byte, len(c.previous))
		c.currentBytes = 0
	}
	c.current[id] = data
	c.currentBytes += len(data)
}

func (c *imageDataCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = map[string][]byte{}
	c.previous = map[string][]byte{}
	c.currentBytes = 0
}
