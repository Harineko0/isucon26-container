package isureal

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"example.com/isureal-company/isureal/internal/jpegcheck"
	"github.com/labstack/echo/v5"
)

// 新しく投稿された画像 (投稿・RealMoji・プロフィール画像) の本体は、受け付けたリージョンのサーバーの
// ディスク (imageStoreDir) に fsync してから DB にメタデータだけを入れる (image_objects.data は空、
// stored_region に置き場のリージョン)。閲覧可否は従来どおり毎回 DB で判定する。
// 手元に無い画像は置き場のリージョンのサーバーから private IP (ルーター経由) で取り、ここに複製する。
// 初期データの画像 (stored_region IS NULL) は従来どおり DB と imageDiskDir にある。
var imageStoreDir = envOrDefault("ISUREAL_IMAGE_STORE_DIR", "/home/isucon/imgstore")

// 各リージョンのアプリサーバーの private IP。通信はルーターを経由する (迂回しない)。
// 手元での確認用に ISUREAL_PEER_HOSTS=asia=127.0.0.1:18081,europe=... で差し替えられる。
var regionPeerHosts = peerHostsFromEnv(map[string]string{
	"asia":    "192.168.1.10",
	"europe":  "192.168.2.10",
	"america": "192.168.3.10",
})

func peerHostsFromEnv(defaults map[string]string) map[string]string {
	for _, kv := range strings.Split(os.Getenv("ISUREAL_PEER_HOSTS"), ",") {
		if region, host, ok := strings.Cut(kv, "="); ok {
			defaults[region] = host
		}
	}
	return defaults
}

// サーバー間の内部 API (/api/internal/...) は、DB のパスワードから作った共有シークレットを
// このヘッダで持つ要求だけを受け付ける。持たない要求は従来どおりのルーティング (未定義の URI) になる。
const internalSecretHeader = "X-Isureal-Internal"

var internalSecret atomic.Pointer[string]

func setInternalSecret(config *Config) {
	mac := hmac.New(sha256.New, []byte(config.DBPassword))
	mac.Write([]byte("isureal-internal-image-store-v1"))
	secret := hex.EncodeToString(mac.Sum(nil))
	internalSecret.Store(&secret)
}

func validInternalSecret(r *http.Request) bool {
	secret := internalSecret.Load()
	got := r.Header.Get(internalSecretHeader)
	return secret != nil && got != "" && hmac.Equal([]byte(got), []byte(*secret))
}

func imageStorePath(id string) (string, bool) {
	if !imageIDPattern.MatchString(id) {
		return "", false
	}
	return filepath.Join(imageStoreDir, id[:2], id), true
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// ensureImageStoreDirs はストアの根と 256 個のシャードを作り、ディレクトリの項目を fsync する。
func ensureImageStoreDirs() error {
	created := false
	if _, err := os.Stat(imageStoreDir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(imageStoreDir, 0o755); err != nil {
			return err
		}
		created = true
	}
	for i := 0; i < 256; i++ {
		shard := filepath.Join(imageStoreDir, fmt.Sprintf("%02x", i))
		if _, err := os.Stat(shard); err == nil {
			continue
		}
		if err := os.Mkdir(shard, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		created = true
	}
	if created {
		if err := syncDir(imageStoreDir); err != nil {
			return err
		}
		return syncDir(filepath.Dir(imageStoreDir))
	}
	return nil
}

// writeImageFileDurable は一時ファイルに書いて fsync → rename → ディレクトリを fsync する
// (応答・DB のコミットより前に呼べば、再起動しても消えない。途中で止まっても壊れたファイルを残さない)。
func writeImageFileDurable(path string, data []byte) error {
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := ensureImageStoreDirs(); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	tmp.Chmod(0o644)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return syncDir(dir)
}

func readImageFromStore(id string) ([]byte, bool) {
	path, ok := imageStorePath(id)
	if !ok {
		return nil, false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, false
	}
	return data, true
}

// storeUploadedImage はアップロードされた画像をこのサーバーのディスクに永続化する。
// 戻り値は DB 関数に渡す data と stored_region。ディスクに書けなければ従来どおり DB に本体を入れる。
func storeUploadedImage(id string, data []byte, region string) ([]byte, *string) {
	path, ok := imageStorePath(id)
	if !ok {
		return data, nil
	}
	if err := writeImageFileDurable(path, data); err != nil {
		fmt.Fprintf(os.Stderr, "imgstore: write %s failed, storing in DB: %v\n", id, err)
		return data, nil
	}
	return []byte{}, &region
}

// discardUploadedImage は DB に入らなかった (ロールバックされた) 画像のファイルを消す。
func discardUploadedImage(id string, storedRegion *string) {
	if storedRegion == nil {
		return
	}
	if path, ok := imageStorePath(id); ok {
		os.Remove(path)
	}
}

// resetImageStore は initialize で、ベンチが投稿した画像のファイルを捨てる (初期データの imageDiskDir は残す)。
// ディレクトリごと退避してから作り直すので、すぐに返る。消すのは裏で行う。
func resetImageStore() error {
	jpegcheck.ResetValidationCache()
	imageDataStore.reset()
	trash := fmt.Sprintf("%s.trash-%d", imageStoreDir, time.Now().UnixNano())
	if err := os.Rename(imageStoreDir, trash); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensureImageStoreDirs(); err != nil {
		return err
	}
	go os.RemoveAll(trash)
	return nil
}

// removeImageStoreTrash は前回の起動で消しきれなかった退避先を消す。
func removeImageStoreTrash() {
	matches, _ := filepath.Glob(imageStoreDir + ".trash-*")
	for _, m := range matches {
		os.RemoveAll(m)
	}
}

var peerHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        64,
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     5 * time.Minute,
	},
}

func peerURL(region, path string) (string, bool) {
	host, ok := regionPeerHosts[region]
	if !ok {
		return "", false
	}
	return "http://" + host + path, true
}

// resetPeerImageStores は initialize を受けたサーバーから、ほかのリージョンのストアも消させる
// (initialize はルーター経由で 1 台にしか来ない)。JPEG 検証キャッシュも各台で消すので、
// 全台から成功の応答を受け取ってから返す。失敗時は initialize も成功にしない。
func resetPeerImageStores(selfRegion string) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(regionPeerHosts))
	for region := range regionPeerHosts {
		if region == selfRegion {
			continue
		}
		wg.Add(1)
		go func(region string) {
			defer wg.Done()
			url, _ := peerURL(region, "/api/internal/images/reset")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
			if err != nil {
				errs <- fmt.Errorf("reset %s: %w", region, err)
				return
			}
			req.Header.Set(internalSecretHeader, *internalSecret.Load())
			resp, err := peerHTTPClient.Do(req)
			if err != nil {
				errs <- fmt.Errorf("reset %s: %w", region, err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusNoContent {
				errs <- fmt.Errorf("reset %s: status %d", region, resp.StatusCode)
			}
		}(region)
	}
	wg.Wait()
	close(errs)
	var all []error
	for err := range errs {
		all = append(all, err)
	}
	return errors.Join(all...)
}

type imageFetchCall struct {
	done chan struct{}
	data []byte
	err  error
}

var (
	imageFetchMu    sync.Mutex
	imageFetchCalls = map[string]*imageFetchCall{}

	imagePullCount atomic.Int64
	imagePullFail  atomic.Int64
	imagePullNanos atomic.Int64
	imagePullBytes atomic.Int64
	imagePushCount atomic.Int64
	imagePushFail  atomic.Int64
)

func init() {
	go func() {
		// 他リージョンからの取得を観測する (stderr → journal。action log には出さない)。
		for range time.Tick(5 * time.Second) {
			n, f := imagePullCount.Swap(0), imagePullFail.Swap(0)
			ns, b := imagePullNanos.Swap(0), imagePullBytes.Swap(0)
			if n > 0 || f > 0 {
				fmt.Fprintf(os.Stderr, "imgpull n=%d fail=%d avg=%s bytes=%d\n", n, f, time.Duration(ns/max(n, 1)), b)
			}
			if pn, pf := imagePushCount.Swap(0), imagePushFail.Swap(0); pn > 0 || pf > 0 {
				fmt.Fprintf(os.Stderr, "imgpush n=%d fail=%d queued=%d\n", pn, pf, len(imagePushQueue))
			}
		}
	}()
}

// loadStoredImage は stored_region に置かれた画像の本体を返す。手元 (メモリ・ディスク) に無ければ、
// 置き場のリージョン → ほかのリージョンの順に取りに行く。同じ id の取得は 1 本にまとめる。
func loadStoredImage(id, storedRegion, selfRegion string) ([]byte, error) {
	if data, ok := imageDataStore.get(id); ok {
		return data, nil
	}
	imageFetchMu.Lock()
	if call, ok := imageFetchCalls[id]; ok {
		imageFetchMu.Unlock()
		<-call.done
		return call.data, call.err
	}
	call := &imageFetchCall{done: make(chan struct{})}
	imageFetchCalls[id] = call
	imageFetchMu.Unlock()

	call.data, call.err = fetchImageFromPeers(id, storedRegion, selfRegion)
	if call.err == nil {
		imageDataStore.put(id, call.data)
	}
	imageFetchMu.Lock()
	delete(imageFetchCalls, id)
	imageFetchMu.Unlock()
	close(call.done)
	if call.err == nil {
		data := call.data
		go func() {
			if path, ok := imageStorePath(id); ok {
				if _, err := os.Stat(path); err == nil {
					return
				}
				writeImageFileDurable(path, data)
			}
		}()
	}
	return call.data, call.err
}

func fetchImageFromPeers(id, storedRegion, selfRegion string) ([]byte, error) {
	order := make([]string, 0, 3)
	if storedRegion != selfRegion {
		order = append(order, storedRegion)
	}
	for _, region := range []string{"asia", "europe", "america"} {
		if region != selfRegion && region != storedRegion {
			order = append(order, region)
		}
	}
	start := time.Now()
	var lastErr error = fmt.Errorf("image %s (stored in %s) not available", id, storedRegion)
	for i, region := range order {
		timeout := 10 * time.Second
		if i > 0 {
			timeout = 5 * time.Second
		}
		data, err := fetchImageFromPeer(region, id, timeout)
		if err == nil {
			imagePullCount.Add(1)
			imagePullNanos.Add(int64(time.Since(start)))
			imagePullBytes.Add(int64(len(data)))
			return data, nil
		}
		lastErr = err
	}
	imagePullFail.Add(1)
	return nil, lastErr
}

func fetchImageFromPeer(region, id string, timeout time.Duration) ([]byte, error) {
	url, ok := peerURL(region, "/api/internal/images/"+id)
	if !ok {
		return nil, fmt.Errorf("unknown region %q", region)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(internalSecretHeader, *internalSecret.Load())
	resp, err := peerHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK || len(data) == 0 {
		return nil, fmt.Errorf("fetch image %s from %s: status %d", id, region, resp.StatusCode)
	}
	return data, nil
}

// InternalMiddleware はルーティングより前 (Pre) で、共有シークレットを持つサーバー間の要求だけを処理する。
// シークレットが無い・違う要求は何もせず次に渡す (従来どおり未定義の URI として扱われる)。
// action log・IP BAN の対象にもしない (利用者の操作ではないため)。
func InternalMiddleware(h *HandlerContext) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			r := c.Request()
			if !strings.HasPrefix(r.URL.Path, "/api/internal/") || !validInternalSecret(r) {
				return next(c)
			}
			path := strings.TrimPrefix(r.URL.Path, "/api/internal/")
			switch {
			case r.Method == http.MethodPost && path == "images/reset":
				if err := resetImageStore(); err != nil {
					fmt.Fprintf(os.Stderr, "imgstore: reset failed: %v\n", err)
					return c.NoContent(http.StatusInternalServerError)
				}
				return c.NoContent(http.StatusNoContent)
			case r.Method == http.MethodPut && strings.HasPrefix(path, "images/"):
				// 他リージョンで受け付けた画像の複製を受け取る (pushStoredImage)。
				id := strings.TrimPrefix(path, "images/")
				dst, ok := imageStorePath(id)
				if !ok {
					return c.NoContent(http.StatusBadRequest)
				}
				if _, err := os.Stat(dst); err == nil {
					return c.NoContent(http.StatusNoContent)
				}
				data, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
				if err != nil || len(data) == 0 {
					return c.NoContent(http.StatusBadRequest)
				}
				if err := writeImageFileDurable(dst, data); err != nil {
					return c.NoContent(http.StatusInternalServerError)
				}
				return c.NoContent(http.StatusNoContent)
			case r.Method == http.MethodGet && strings.HasPrefix(path, "images/"):
				// 手元 (メモリ・ディスク) にあるものだけを返す。ここからさらに他のサーバーへは取りに行かない。
				data, ok := imageDataStore.get(strings.TrimPrefix(path, "images/"))
				if !ok {
					return c.NoContent(http.StatusNotFound)
				}
				return c.Blob(http.StatusOK, "application/octet-stream", data)
			}
			return next(c)
		}
	}
}

// 新しい画像の id の末尾 2 文字に置き場のリージョンを入れておき、DB の応答を待たずに取りに行く手がかりにする。
// 正しさ (閲覧可否・置き場) は DB で判断する。手がかりが外れても余計な取得が裏で 1 回起きるだけ。
var regionImageIDTags = map[string]string{"asia": "f1", "europe": "f2", "america": "f3"}

func newImageID(region string) string {
	// 時刻先頭の ID (ids.go の newTimeID、索引への追記を局所化する) の末尾 2 文字を置き場のリージョンの印にする。
	id := newTimeID()
	if tag, ok := regionImageIDTags[region]; ok {
		id = id[:len(id)-len(tag)] + tag
	}
	return id
}

func imageRegionHint(id string) (string, bool) {
	if !imageIDPattern.MatchString(id) {
		return "", false
	}
	for region, tag := range regionImageIDTags {
		if strings.HasSuffix(id, tag) {
			return region, true
		}
	}
	return "", false
}

// prefetchStoredImage は手元に無い他リージョンの画像を、DB での閲覧可否の確認と並行して取りに行く
// (取った本体はメモリに入るだけで、返すかどうかは DB の結果で決める)。
func prefetchStoredImage(id, selfRegion string) {
	if region, ok := imageRegionHint(id); ok && region != selfRegion {
		go loadStoredImage(id, region, selfRegion)
	}
}

// 閲覧の大半はアジアなので、ヨーロッパ・アメリカで受け付けた画像はアジアのサーバーへ裏で複製しておく
// (今まで DB (asia2) へ同期で送っていたのと同じ量を、応答の後に送る)。複製が間に合わなければ、
// アジアは従来どおり置き場から取りに行く。キューがあふれたら捨てる。
const imageReplicaRegion = "asia"

type imagePush struct {
	id   string
	data []byte
}

var imagePushQueue = make(chan imagePush, 1024)

func startImagePushers(selfRegion string) {
	if selfRegion == imageReplicaRegion {
		return
	}
	for range 4 {
		go func() {
			for p := range imagePushQueue {
				pushStoredImage(p)
			}
		}()
	}
}

func enqueueImagePush(id string, data []byte, storedRegion *string, selfRegion string) {
	if storedRegion == nil || selfRegion == imageReplicaRegion {
		return
	}
	select {
	case imagePushQueue <- imagePush{id: id, data: data}:
	default:
	}
}

func pushStoredImage(p imagePush) {
	url, ok := peerURL(imageReplicaRegion, "/api/internal/images/"+p.id)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(p.data))
	if err != nil {
		return
	}
	req.Header.Set(internalSecretHeader, *internalSecret.Load())
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := peerHTTPClient.Do(req)
	if err != nil {
		imagePushFail.Add(1)
		return
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		imagePushCount.Add(1)
	} else {
		imagePushFail.Add(1)
	}
}
