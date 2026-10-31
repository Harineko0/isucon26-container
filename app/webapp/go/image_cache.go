package isureal

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmoiron/sqlx"
)

// 初期データの画像は cmd/imgsync でローカルディスクに書き出してある (不変なので内容は DB と同じ)。
// 手元での確認用に ISUREAL_IMAGE_CACHE_DIR で差し替えられる (nginx の alias も合わせること)。
var imageDiskDir = envOrDefault("ISUREAL_IMAGE_CACHE_DIR", "/home/isucon/imgcache")

// ディスクにある画像の本体は Go が読まず、nginx に X-Accel-Redirect で返させる (image_api.go)。
// nginx 側の internal location (infra/app/etc/nginx/sites-available/isureal.conf) が
// imageDiskDir / imageStoreDir を alias で指している。ISUREAL_IMAGE_ACCEL=0 で止められる
// (止めると従来どおり Go が読んで返す)。
var imageAccelEnabled = envOrDefault("ISUREAL_IMAGE_ACCEL", "1") != "0"

const (
	imageDiskAccelPrefix  = "/_imgcache/"
	imageStoreAccelPrefix = "/_imgstore/"
)

// imageFileExists はディスクの画像ファイルが読める状態 (長さ > 0) かを stat だけで確かめる。
// 長さ 0 は書きかけ (強制停止など) とみなす。一時ファイルは別名なので、見えるのは書き終えたものだけ。
func imageFileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular() && fi.Size() > 0
}

// locate は画像の本体の在り処を返す。メモリにあれば data、ディスク (imgcache / imgstore) にあれば
// nginx に渡す X-Accel-Redirect の URI (本体は読まない)。どちらも無ければ ok=false。
// ディスクのファイルはメモリには載せない (Go のヒープを使わず、OS のページキャッシュに任せる)。
func (c *imageDataCache) locate(id string) (data []byte, accelURI string, ok bool) {
	c.mu.RLock()
	data, ok = c.current[id]
	if !ok {
		data, ok = c.previous[id]
	}
	c.mu.RUnlock()
	if ok && data != nil {
		return data, "", true
	}
	if !imageAccelEnabled {
		data, ok = c.get(id)
		return data, "", ok
	}
	if path, ok := imageDiskPath(id); ok && imageFileExists(path) {
		return nil, imageDiskAccelPrefix + id[:2] + "/" + id, true
	}
	if path, ok := storedImagePath(id); ok {
		return nil, imageStoreAccelPrefix + filepath.Base(filepath.Dir(path)) + "/" + id, true
	}
	return nil, "", false
}

var imageIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func imageDiskPath(id string) (string, bool) {
	if !imageIDPattern.MatchString(id) {
		return "", false
	}
	return filepath.Join(imageDiskDir, id[:2], id), true
}

// WriteImageToDisk は画像を一時ファイル経由で書き出す (途中で止まっても壊れたファイルを残さない)。
// 一時ファイルは書き手ごとに別名にし、fsync してから rename する
// (同じ id を同時に書いても、強制停止のあとでも、空や途中までのファイルが見えることはない)。
func WriteImageToDisk(id string, data []byte) error {
	path, ok := imageDiskPath(id)
	if !ok || len(data) == 0 {
		return nil
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, id+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

func readImageFromDisk(id string) ([]byte, bool) {
	path, ok := imageDiskPath(id)
	if !ok {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		// 長さ 0 は書きかけ (強制停止など) とみなして DB から取り直す。
		return nil, false
	}
	return data, true
}

// initialDataBoundaryMicros は initial_data_boundary.created_at (UnixMicro)。0 なら未取得で、ディスクへは書き戻さない。
var initialDataBoundaryMicros atomic.Int64

// loadInitialDataBoundary は初期データの境界を読む (起動時と initialize 後。値は変わらないが念のため読み直す)。
func loadInitialDataBoundary(db *sqlx.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var boundary time.Time
	if err := db.GetContext(ctx, &boundary, `SELECT created_at FROM initial_data_boundary`); err != nil {
		fmt.Fprintf(os.Stderr, "imgcache: failed to load initial_data_boundary: %v\n", err)
		return
	}
	initialDataBoundaryMicros.Store(boundary.UnixMicro())
}

// DB から取った画像のうち初期データのもの (created_at <= 境界) は、ディスクにも書き戻す。
// 初期データの画像は id ごとに不変で initialize でも消えないので、cmd/imgsync で埋めきれなかった分が
// ベンチ中に埋まり、以後 (再起動後も) DB から運ばずに済む。ベンチ中に投稿された画像は書かない (ディスクが際限なく増えるため)。
// 書き込みは別 goroutine で行い、キューが埋まっていれば諦める (リクエストを待たせない。次に DB から取ったときにまた試す)。
// 閲覧可否はこれまでどおり毎回 DB で確認するので、ディスクにあるだけの画像を返すことはない。
const imageDiskWriteQueue = 1024

type imageDiskWrite struct {
	id   string
	data []byte
}

var (
	imageDiskWrites  = make(chan imageDiskWrite, imageDiskWriteQueue)
	imageDiskPending sync.Map // 書き込み待ち・書き込み中の id (同じ画像を重ねて積まない)
	imageDiskWriters sync.Once
)

func saveInitialImageToDisk(id string, createdAt time.Time, data []byte) {
	boundary := initialDataBoundaryMicros.Load()
	if boundary == 0 || createdAt.IsZero() || createdAt.UnixMicro() > boundary || len(data) == 0 {
		return
	}
	imageDiskWriters.Do(func() {
		for range 2 {
			go func() {
				for w := range imageDiskWrites {
					if err := WriteImageToDisk(w.id, w.data); err != nil {
						fmt.Fprintf(os.Stderr, "imgcache: failed to write %s: %v\n", w.id, err)
					}
					imageDiskPending.Delete(w.id)
				}
			}()
		}
	})
	if _, loaded := imageDiskPending.LoadOrStore(id, struct{}{}); loaded {
		return
	}
	select {
	case imageDiskWrites <- imageDiskWrite{id: id, data: data}:
	default:
		imageDiskPending.Delete(id)
	}
}

// imageCreatedAt は画像 id → created_at (不変)。パーティションを絞るために使う。
// 値は created_at の上限 (手がかり) で、画像の created_at は [値 - 1 時間, 値] に入る
// (投稿画像と画像取得で得た値は created_at そのもの、RealMoji は realmojis.created_at:
// 画像は同じトランザクションでそれより先に INSERT される)。外れていても id だけで引き直すので結果は変わらない。
// 1 件 150 バイト程度。上限を超えて捨てても id だけの検索に戻るだけ。
var imageCreatedAt = &boundedMap{limit: 2_000_000}

// 画像の本体は id ごとに不変なので、リージョンごとにメモリへ置いて DB からの転送を省く。
// 閲覧可否は毎回 DB で確認するので、ここに残った削除済み画像が返ることはない。
// 2 世代で最大 384MiB。追い出された画像はディスクの imgcache (ページキャッシュ) から戻る。
const imageDataGenerationBytes = 192 << 20

// これより大きい画像は 1 枚で世代を押し出すのでメモリに置かない。
const imageDataMaxEntryBytes = imageDataGenerationBytes / 8

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
	if data, ok := readImageFromStore(id); ok {
		c.put(id, data)
		return data, true
	}
	return nil, false
}

func (c *imageDataCache) put(id string, data []byte) {
	if data == nil || len(data) > imageDataMaxEntryBytes {
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
