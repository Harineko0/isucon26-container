package isureal

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// 投稿・RealMoji・プロフィール画像の本体は、要求の中では一時ファイルに書いて rename するところまでで止め、
// fsync (ファイルと親ディレクトリ) はこの syncer が要求の外でまとめて行う (write-behind)。
//
// 理由: アプリサーバーの EBS は gp3 の既定値 (3,000 IOPS / 125 MB/s。contest_cloudformation.yaml の
// アプリ側の LaunchTemplate は Iops / Throughput を指定していない) で、最終日の asia は 1,044 人が通知の
// 直後に一斉に投稿する。1 件 355 KB の投稿画像を要求ごとに fsync すると、約 370 MB の書き込みが EBS の
// 125 MB/s に並び、後ろの投稿ほど数秒を fsync で待っていた (job #33588: asia の POST /api/posts 平均 3.1 秒・
// 最大 9.7 秒、PUT /api/me/profile-image 最大 14.3 秒、asia1 の wa 17%)。rename まで済めば画像は
// ページキャッシュから読めるので、閲覧 (nginx の X-Accel-Redirect / readImageFromStore) は fsync を待つ必要がない。
// 書き込みの総量は変わらないが、要求はディスクの順番待ちから外れ、EBS は裏で書き出しを続ける。
//
// 耐久性: rename 直後のファイルはカーネルのページキャッシュに dirty として残り、この syncer が
// imageSyncInterval 以内 (混雑時は EBS が書ける速さで) に fsync する。プロセスが落ちても dirty ページは
// カーネルに残り、再起動・systemctl stop (正常な shutdown) でも書き出される。SIGTERM/SIGINT を受けたときは
// 溜まった分を fsync してから終了する (flushOnTerminate)。失われうるのはカーネルごと止まる (電源断・カーネル
// パニック) 場合の、まだ fsync していない分だけで、追試の再起動 (正常な shutdown) では従来と差が出ない。
// DB (asia2) の fsync / synchronous_commit は変えていない。
//
// 消えたファイル: DB に入らなかった画像 (discardUploadedImage) や initialize の退避 (resetImageStore) で
// rename 後に消えたファイルは fsync できないが、消えてよいものなので数えない。
const (
	// 落ち着いているときに fsync を始めるまでの最大の待ち。
	imageSyncInterval = 50 * time.Millisecond
	// これだけ溜まったら待たずに始める。ext4 では 1 回目の fsync が同じ journal transaction の分をまとめて
	// 書き出すので、まとまった数を 1 度に渡すほど journal commit の回数が減る。
	imageSyncBatch = 64
)

type pendingImageFile struct {
	path string
	at   time.Time
}

type imageFileSyncer struct {
	mu      sync.Mutex
	pending []pendingImageFile
	wake    chan struct{}

	// syncing は fsync の 1 回分を直列にする。flush はこれを取ることで、進行中の分が終わるのも待つ。
	syncing sync.Mutex

	// テストで差し替える。
	fsyncFile func(path string) error
	fsyncDir  func(dir string) error

	synced atomic.Int64 // fsync まで終えたファイル (5 秒ごとの報告でリセット)
	failed atomic.Int64 // fsync に失敗したファイル (消されていたものは数えない)
	lagMax atomic.Int64 // enqueue から fsync 完了までの最大 (ns)
}

func newImageFileSyncer() *imageFileSyncer {
	return &imageFileSyncer{
		wake:      make(chan struct{}, 1),
		fsyncFile: fsyncPath,
		fsyncDir:  syncDir,
	}
}

var imageSyncer = newImageFileSyncer()

// fsyncPath はパスのファイルを開いて fsync する。rename 後に消されていれば os.ErrNotExist。
func fsyncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// enqueue は rename まで済んだファイルを fsync の対象に加える。
func (s *imageFileSyncer) enqueue(path string) {
	s.mu.Lock()
	s.pending = append(s.pending, pendingImageFile{path: path, at: time.Now()})
	n := len(s.pending)
	s.mu.Unlock()
	if n >= imageSyncBatch {
		s.kick()
	}
}

func (s *imageFileSyncer) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *imageFileSyncer) pendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// run は imageSyncInterval ごと、または imageSyncBatch 件溜まるごとに、溜まった分を fsync する。
func (s *imageFileSyncer) run() {
	ticker := time.NewTicker(imageSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
		case <-s.wake:
		}
		s.flush()
	}
}

// flush は、呼んだ時点までに enqueue されたファイルがすべて fsync まで終わるのを待つ (進行中の分も含む)。
// ファイルを fsync してから、それらの親ディレクトリを (重複を除いて) fsync する。
func (s *imageFileSyncer) flush() {
	s.syncing.Lock()
	defer s.syncing.Unlock()
	s.mu.Lock()
	batch := s.pending
	s.pending = nil
	s.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	dirs := make(map[string]struct{}, 4)
	var synced int64
	for _, p := range batch {
		if err := s.fsyncFile(p.path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				s.failed.Add(1)
				fmt.Fprintf(os.Stderr, "imgstore: fsync %s failed: %v\n", p.path, err)
			}
			continue
		}
		synced++
		dirs[filepath.Dir(p.path)] = struct{}{}
	}
	for dir := range dirs {
		if err := s.fsyncDir(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "imgstore: fsync dir %s failed: %v\n", dir, err)
		}
	}
	s.synced.Add(synced)
	now := time.Now()
	for _, p := range batch {
		if lag := int64(now.Sub(p.at)); lag > s.lagMax.Load() {
			s.lagMax.Store(lag)
		}
	}
}

var startImageSyncerOnce sync.Once

// startImageFileSyncer は syncer を起動する (NewServer から 1 回)。テストは imageSyncer を差し替えて flush を直接呼ぶ。
func startImageFileSyncer() {
	startImageSyncerOnce.Do(func() {
		go imageSyncer.run()
		go imageSyncer.report()
		go imageSyncer.flushOnTerminate()
	})
}

// report は fsync の進み具合を 5 秒ごとに stderr (journal) に出す。imgpull / imgpush と同じく action log には出さない。
func (s *imageFileSyncer) report() {
	for range time.Tick(5 * time.Second) {
		n, f, lag := s.synced.Swap(0), s.failed.Swap(0), s.lagMax.Swap(0)
		if n > 0 || f > 0 {
			fmt.Fprintf(os.Stderr, "imgfsync n=%d fail=%d lagmax=%s pending=%d\n", n, f, time.Duration(lag), s.pendingCount())
		}
	}
}

// flushOnTerminate は SIGTERM/SIGINT (systemctl stop / restart、デプロイ) で、溜まった分を fsync してから
// 既定の動作 (終了) に戻す。fsync が終わらないうちに止まっても dirty ページはカーネルが書き出す。
func (s *imageFileSyncer) flushOnTerminate() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	sig := <-signals
	s.flush()
	signal.Reset(sig)
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		_ = p.Signal(sig)
	}
	time.Sleep(time.Second)
	os.Exit(1)
}
