// Package scenario は公式ベンチの負荷走行を再現する。
//
// 公式ベンチの stdout (CI の PR コメントに残る) から読み取れた流れ:
//
//	asia 1 日目 → (3 秒後) europe 1 日目 → (3 秒後) america 1 日目 → (asia から 9 秒後) asia 2 日目 …
//	利用者は日ごとに約 3 倍 (asia 18 → 54 → 160 → 468 → 1155)。増え方は前日の検証通過数に比例する。
//	5 日目のあと、3 リージョン同時に「最終日（グランプリ）」が開き、約 8 秒後に
//	「グランプリ: アジア 924 人, ヨーロッパ 324 人, アメリカ 283 人」(最終日の検証通過数) が出て終わる。
//	途中で "user moved" (リージョン移動) が数件ある。
//
// 1 利用者の 1 日は「通知を購読 → 投稿期間が開く → 投稿 → 直後の確認 → 友達のフィードで検証 → 閲覧・交流」。
// 加点は manual.md「スコアの計算」の配点表と上限に従う (score パッケージ)。
package scenario

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"isureal-bench/internal/api"
	"isureal-bench/internal/client"
	"isureal-bench/internal/jpegs"
	"isureal-bench/internal/manifest"
	"isureal-bench/internal/score"
)

type Config struct {
	Days              int            // 通常の投稿期間の日数 (公式 5)
	WindowInterval    time.Duration  // 同じリージョンで次の投稿期間が開くまで (公式 9 秒)
	Stagger           time.Duration  // リージョン間のずれ (公式 3 秒)
	GrandPrix         bool           // 最終日 (全リージョン同時、20 点) を行う
	GrandPrixDuration time.Duration  // 最終日の長さ (公式 約 8 秒)
	OnTimeSeconds     int            // 投稿期間の onTimeDeadline (開始からの秒)
	InitialUsers      map[string]int // manifest に Seed 利用者が無いときに日 1 で登録する人数
	GrowthFactor      int            // 検証通過 1 人あたり翌日に増える人数 (公式は約 2)
	GrowthSnapshot    time.Duration  // 翌日の人数を決める時点 (投稿期間の開始から)
	MaxUsersPerRegion int            // 0 なら無制限
	ThinkTime         time.Duration  // 閲覧操作の間隔
	PostImageBytes    int            // 投稿画像の大きさの目安
	RealMojiBytes     int
	ImagePoolSize     int
	RegisterParallel  int     // 登録の並列数
	MaxErrorRate      float64 // これを超えると FAIL
	MaxMessages       int
	Seed              uint64
	// 削除した投稿の画像が 403 になるまでの猶予 (manual.md: 5 秒)
	DeletionGrace time.Duration
	// 1 利用者が同時に出す閲覧要求数 (ブラウザのタブ相当)。1 なら逐次
	// 利用者ごとの行動確率 (0〜1)
	LogoutRate, MoveRate, DeleteAccountRate, DeletePostRate, ExportRate, ProfileImageRate, ProfileUpdateRate, BlockRate, NegativeCheckRate float64
}

func DefaultConfig() Config {
	return Config{
		Days:              5,
		WindowInterval:    9 * time.Second,
		Stagger:           3 * time.Second,
		GrandPrix:         true,
		GrandPrixDuration: 8 * time.Second,
		OnTimeSeconds:     5,
		InitialUsers:      map[string]int{"asia": 18, "europe": 7, "america": 6},
		GrowthFactor:      2,
		GrowthSnapshot:    5 * time.Second,
		ThinkTime:         200 * time.Millisecond,
		PostImageBytes:    350 * 1024,
		RealMojiBytes:     30 * 1024,
		ImagePoolSize:     24,
		RegisterParallel:  32,
		MaxErrorRate:      0.05,
		MaxMessages:       200,
		DeletionGrace:     5 * time.Second,
		LogoutRate:        0.3,
		MoveRate:          0.004,
		DeleteAccountRate: 0.01,
		DeletePostRate:    0.02,
		ExportRate:        0.12,
		ProfileImageRate:  0.2,
		ProfileUpdateRate: 0.15,
		BlockRate:         0.01,
		NegativeCheckRate: 0.1,
	}
}

// Post はベンチが把握している投稿 (自分たちが作ったものと manifest の初期データ)。
type Post struct {
	ID         string
	AuthorID   string
	WindowID   string
	LocalDate  string
	Region     string // 投稿したリージョン
	ImageURL   string
	ImageSHA   [32]byte
	HasSHA     bool
	Caption    string
	Visibility string
	PostedAt   time.Time
	deletedAt  atomic.Int64 // UnixNano。0 なら未削除
	// ベンチが PATCH でキャプションを変えた (変更後の読み取りの食い違いは致命にせず数える)
	captionUpdated atomic.Bool
}

func (p *Post) Deleted() bool { return p.deletedAt.Load() != 0 }
func (p *Post) DeletedBefore(t time.Time) bool {
	d := p.deletedAt.Load()
	return d != 0 && d < t.UnixNano()
}

type User struct {
	ID          string
	Username    string
	SecretKey   string
	DisplayName string
	Bio         string
	AccountType string
	ClientIP    string
	Initial     bool // manifest の初期ユーザー

	mu          sync.Mutex
	region      string
	sess        *client.Session
	friends     map[string]*User
	everFriends map[string]bool
	following   map[string]*User
	blocked     map[string]time.Time // 自分がブロックした相手 → ブロックした時刻 (初期データは零値)
	blockedBy   map[string]time.Time
	posts       []*Post // 自分の投稿 (新しい順ではない。追加順)
	deleted     bool
	deletedAt   atomic.Int64 // UnixNano
	loggedOut   bool
	// 直近に見た投稿 (閲覧の候補)
	seen []*Post
	// 投稿期間ごとの加点カウンタ (window ID → counter)
	windowCounters  map[string]*score.WindowCounter
	verifiedWindows map[string]bool
	// 検証通過数 (成長の計算に使う)
	passedDays       int
	profileImageSHA  [32]byte
	hasProfileImage  bool
	deletedImageURLs []deletedImage
	busy             atomic.Int32
	// 投稿の要求が応答を得られずに終わった (サーバ側では作られたかもしれない)。以後、投稿数の一致は見ない
	uncertainPosts   atomic.Bool
	profileChangedAt atomic.Int64 // UnixNano。表示名・自己紹介を変え始めた時刻 (他の利用者の読み取りと競合する)
	loginMu          sync.Mutex
	lastLogin        atomic.Int64 // UnixNano。ログイン・ログアウト・移動の時刻
	// 通知の購読 (SSE) は利用者ごとに 1 本を張り続け、届いた通知をここに入れる
	notifyCh     chan *api.Notification
	notifierOnce sync.Once
}

type deletedImage struct {
	URL       string
	DeletedAt time.Time
}

func (u *User) Region() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.region
}

func (u *User) Session() *client.Session {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.sess
}

func (u *User) setSession(token string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if token == "" {
		u.sess = nil
		u.loggedOut = true
		return
	}
	u.loggedOut = false
	u.sess = &client.Session{Token: token, Country: countryFor(u.region, u.ID), ClientIP: u.ClientIP}
	u.lastLogin.Store(time.Now().UnixNano())
}

// sessionJustChanged は直近 3 秒にログイン・ログアウト・退会があったか (他の利用者の goroutine が
// 古いトークンで送って 401 になることがあり、それはアプリの不具合ではない)。
func (u *User) sessionJustChanged() bool {
	return u.LoggedOut() || time.Since(time.Unix(0, u.lastLogin.Load())) < 3*time.Second
}

func (u *User) LoggedOut() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.loggedOut || u.sess == nil
}

func (u *User) Deleted() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.deleted
}

func (u *User) IsFriend(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, ok := u.friends[id]
	return ok
}

func (u *User) EverFriend(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.everFriends[id]
}

func (u *User) IsBlockedEither(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, a := u.blocked[id]
	_, b := u.blockedBy[id]
	return a || b
}

// BlockedEitherBefore は t より前からブロック関係にあったか (t 以降に成立したものは数えない)。
func (u *User) BlockedEitherBefore(id string, t time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	if at, ok := u.blocked[id]; ok && at.Before(t) {
		return true
	}
	if at, ok := u.blockedBy[id]; ok && at.Before(t) {
		return true
	}
	return false
}

// DeletedBefore は t より前に退会していたか。
func (u *User) DeletedBefore(t time.Time) bool {
	d := u.deletedAt.Load()
	return d != 0 && d < t.UnixNano()
}

func (u *User) Friends() []*User {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]*User, 0, len(u.friends))
	for _, f := range u.friends {
		out = append(out, f)
	}
	return out
}

func (u *User) IsFollowing(id string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, ok := u.following[id]
	return ok
}

func (u *User) Following() []*User {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]*User, 0, len(u.following))
	for _, f := range u.following {
		out = append(out, f)
	}
	return out
}

func (u *User) Posts() []*Post {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]*Post(nil), u.posts...)
}

func (u *User) addPost(p *Post) {
	u.mu.Lock()
	u.posts = append(u.posts, p)
	u.mu.Unlock()
}

// CurrentPost は指定の投稿期間への (削除されていない) 投稿。
func (u *User) PostInWindow(windowID string) *Post {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := len(u.posts) - 1; i >= 0; i-- {
		if u.posts[i].WindowID == windowID && !u.posts[i].Deleted() {
			return u.posts[i]
		}
	}
	return nil
}

func (u *User) PostOnDate(localDate string) *Post {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := len(u.posts) - 1; i >= 0; i-- {
		if u.posts[i].LocalDate == localDate && !u.posts[i].Deleted() {
			return u.posts[i]
		}
	}
	return nil
}

func (u *User) counter(windowID string) *score.WindowCounter {
	u.mu.Lock()
	defer u.mu.Unlock()
	c := u.windowCounters[windowID]
	if c == nil {
		c = score.NewWindowCounter()
		u.windowCounters[windowID] = c
	}
	return c
}

func (u *User) remember(posts ...*Post) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, p := range posts {
		if p.AuthorID == u.ID {
			continue
		}
		u.seen = append(u.seen, p)
	}
	if len(u.seen) > 64 {
		u.seen = u.seen[len(u.seen)-64:]
	}
}

func (u *User) pickSeen(rng *rand.Rand) *Post {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := 0; i < 4 && len(u.seen) > 0; i++ {
		p := u.seen[rng.IntN(len(u.seen))]
		if !p.Deleted() {
			return p
		}
	}
	return nil
}

func makeFriends(a, b *User) {
	// ロック順は ID 順 (デッドロック回避)
	first, second := a, b
	if second.ID < first.ID {
		first, second = second, first
	}
	first.mu.Lock()
	second.mu.Lock()
	a.friends[b.ID] = b
	b.friends[a.ID] = a
	a.everFriends[b.ID] = true
	b.everFriends[a.ID] = true
	second.mu.Unlock()
	first.mu.Unlock()
}

func markEverFriends(a, b *User) {
	first, second := a, b
	if second.ID < first.ID {
		first, second = second, first
	}
	first.mu.Lock()
	second.mu.Lock()
	a.everFriends[b.ID] = true
	b.everFriends[a.ID] = true
	second.mu.Unlock()
	first.mu.Unlock()
}

func unfriend(a, b *User) {
	first, second := a, b
	if second.ID < first.ID {
		first, second = second, first
	}
	first.mu.Lock()
	second.mu.Lock()
	delete(a.friends, b.ID)
	delete(b.friends, a.ID)
	second.mu.Unlock()
	first.mu.Unlock()
}

// Window は 1 リージョンの 1 投稿期間。
type Window struct {
	Region    string
	Day       int
	Final     bool
	LocalDate string
	ID        string
	StartsAt  time.Time // ベンチの時計
	Deadline  time.Time
	EndsAt    time.Time // 次の投稿期間が開く予定 (この時刻で閲覧を止める)
	ready     chan struct{}
	endedAt   atomic.Int64
	passed    atomic.Int64 // 友達フィードの検証通過数
	posted    atomic.Int64
	users     atomic.Int64
	failed    atomic.Bool // 開けなかった
}

func (w *Window) Ended() bool { return w.endedAt.Load() != 0 }
func (w *Window) EndedBefore(t time.Time) bool {
	e := w.endedAt.Load()
	return e != 0 && e < t.UnixNano()
}

type regionState struct {
	mu            sync.Mutex
	users         []*User
	current       *Window // 開いている (ベンチが開いた) 投稿期間
	lastLocalDate time.Time
	// manifest も -start-date も無いとき true。今の投稿期間から翌日を決め、409 なら先の日付を探す (runDay)
	autoDate  bool
	certified []*User
	day       int
}

type World struct {
	cfg    Config
	client *client.Client
	log    *slog.Logger
	board  *score.Board
	man    *manifest.Manifest
	admin  *client.Session

	rngMu sync.Mutex
	rng   *rand.Rand

	usersMu    sync.RWMutex
	users      map[string]*User // ID → user (ベンチが知る全利用者)
	byUsername map[string]*User
	postsMu    sync.RWMutex
	posts      map[string]*Post
	windows    sync.Map // window ID → *Window
	regions    map[string]*regionState

	postImages     *jpegs.Pool
	realmojiImages *jpegs.Pool
	profileImages  *jpegs.Pool

	msgMu          sync.Mutex
	messages       []string
	errors         atomic.Int64
	critical       atomic.Bool
	criticalReason string
	cancelLoad     context.CancelFunc

	loadEnded    atomic.Bool
	lateFeed     atomic.Int64 // 友達フィードの検証で、1 回目に見えず再試行した回数
	staleCaption atomic.Int64 // キャプション変更後の読み取りで古い値が返った回数
	staleProfile atomic.Int64 // 表示名・自己紹介の変更後に別の利用者が古い値を読んだ回数
	loadCtx      context.Context
	usernameSeq  atomic.Int64
	ipSeq        atomic.Int64
	moved        atomic.Int64
}

func New(cfg Config, c *client.Client, log *slog.Logger, man *manifest.Manifest) *World {
	rng := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))
	w := &World{
		cfg: cfg, client: c, log: log, board: score.NewBoard(), man: man, rng: rng,
		users: map[string]*User{}, byUsername: map[string]*User{}, posts: map[string]*Post{},
		regions: map[string]*regionState{},
	}
	for _, r := range api.Regions {
		w.regions[r] = &regionState{}
	}
	return w
}

func (w *World) Board() *score.Board { return w.board }

func (w *World) Messages() []string {
	w.msgMu.Lock()
	defer w.msgMu.Unlock()
	return append([]string(nil), w.messages...)
}

func (w *World) Errors() int64 { return w.errors.Load() }
func (w *World) Critical() (bool, string) {
	w.msgMu.Lock()
	defer w.msgMu.Unlock()
	return w.critical.Load(), w.criticalReason
}

func (w *World) random() *rand.Rand {
	// 利用者ごとに独立した rng を切り出す
	w.rngMu.Lock()
	defer w.rngMu.Unlock()
	return rand.New(rand.NewPCG(w.rng.Uint64(), w.rng.Uint64()))
}

func (w *World) chance(rng *rand.Rand, p float64) bool { return rng.Float64() < p }

// report はエラーを記録する。critical なら負荷を打ち切る。
func (w *World) report(critical bool, u *User, what string, err error) {
	var ce *client.Error
	if !critical && errors.As(err, &ce) && ce.Canceled {
		return // ベンチ側が打ち切った要求 (投稿期間の終わり・負荷の終了) はエラーにしない
	}
	if !critical && u != nil && errors.As(err, &ce) && ce.Got == 401 && u.sessionJustChanged() {
		return // 別の goroutine がログインし直した・ログアウトした直後の古いトークン
	}
	w.errors.Add(1)
	prefix := ""
	if u != nil {
		prefix = u.Username + ": "
	}
	msg := fmt.Sprintf("%s%s: %v", prefix, what, err)
	if critical {
		msg = "[critical] " + msg
	}
	w.msgMu.Lock()
	if len(w.messages) < w.cfg.MaxMessages {
		w.messages = append(w.messages, msg)
	}
	if critical && !w.critical.Load() {
		w.critical.Store(true)
		w.criticalReason = msg
	}
	w.msgMu.Unlock()
	if critical {
		w.log.Error("critical error", "msg", msg)
		if w.cancelLoad != nil {
			w.cancelLoad()
		}
	} else {
		w.log.Debug("error", "msg", msg)
	}
}

func (w *World) addUser(u *User) {
	w.usersMu.Lock()
	w.users[u.ID] = u
	w.byUsername[u.Username] = u
	w.usersMu.Unlock()
	rs := w.regions[u.Region()]
	rs.mu.Lock()
	rs.users = append(rs.users, u)
	rs.mu.Unlock()
}

func (w *World) user(id string) *User {
	w.usersMu.RLock()
	defer w.usersMu.RUnlock()
	return w.users[id]
}

func (w *World) addPost(p *Post) {
	w.postsMu.Lock()
	w.posts[p.ID] = p
	w.postsMu.Unlock()
}

func (w *World) post(id string) *Post {
	w.postsMu.RLock()
	defer w.postsMu.RUnlock()
	return w.posts[id]
}

func (w *World) window(id string) *Window {
	v, ok := w.windows.Load(id)
	if !ok {
		return nil
	}
	return v.(*Window)
}

// activeUsers はリージョンの (退会していない) 利用者の写し。
func (w *World) activeUsers(region string) []*User {
	rs := w.regions[region]
	rs.mu.Lock()
	defer rs.mu.Unlock()
	out := make([]*User, 0, len(rs.users))
	for _, u := range rs.users {
		if !u.Deleted() && u.Region() == region {
			out = append(out, u)
		}
	}
	return out
}

func (w *World) allActiveUsers() []*User {
	var out []*User
	for _, r := range api.Regions {
		out = append(out, w.activeUsers(r)...)
	}
	return out
}

func (w *World) moveUser(u *User, from, to string) {
	rs := w.regions[to]
	rs.mu.Lock()
	rs.users = append(rs.users, u)
	rs.mu.Unlock()
	u.mu.Lock()
	u.region = to
	if u.sess != nil {
		u.sess = &client.Session{Token: u.sess.Token, Country: countryFor(to, u.ID), ClientIP: u.ClientIP}
	}
	u.mu.Unlock()
	u.lastLogin.Store(time.Now().UnixNano())
	w.moved.Add(1)
	w.log.Info("user moved", "user", u.Username, "from", from, "to", to)
}

func countryFor(region, id string) string {
	codes := api.CountryCodes[region]
	if len(codes) == 0 {
		return "JP"
	}
	var h uint32
	for i := 0; i < len(id); i++ {
		h = h*31 + uint32(id[i])
	}
	return codes[h%uint32(len(codes))]
}

func (w *World) newClientIP() string {
	n := w.ipSeq.Add(1)
	return fmt.Sprintf("10.%d.%d.%d", (n>>16)&0xff, (n>>8)&0xff, n&0xff)
}

func (w *World) newUsername(rng *rand.Rand) string {
	n := w.usernameSeq.Add(1)
	return fmt.Sprintf("%s%s%d", syllables[rng.IntN(len(syllables))]+syllables[rng.IntN(len(syllables))], []string{"_", ".", "-", ""}[rng.IntN(4)], 10000+n)
}

var syllables = []string{"ka", "ki", "ku", "ke", "ko", "sa", "shi", "su", "se", "so", "ta", "chi", "tsu", "te", "to", "na", "ni", "nu", "ne", "no", "ha", "hi", "fu", "he", "ho", "ma", "mi", "mu", "me", "mo", "ya", "yu", "yo", "ra", "ri", "ru", "re", "ro", "wa", "dan", "mato", "ruta", "taka", "leo", "max"}

var displayNames = []string{"小林 悠真", "佐藤 さくら", "Alex Johnson", "Marie Dubois", "Lukas Müller", "Sofia Rossi", "田中 太郎", "Emma Brown", "Noah Davis", "Mia Wilson", "山田 花子", "Oliver Smith"}

func (w *World) sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

func (w *World) think(ctx context.Context, rng *rand.Rand) bool {
	d := w.cfg.ThinkTime/2 + time.Duration(rng.Int64N(int64(w.cfg.ThinkTime)+1))
	select {
	case <-time.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

// captionMismatch はキャプションの食い違いを扱う。ベンチが変更した投稿なら致命にせず数えて false を返し、
// そうでなければ (投稿時のまま) true を返す。公式は初期データの期待値との一致を見る (manual.md) ので、
// 変更直後の反映の遅れをどこまで許すかは分からない。
func (w *World) captionMismatch(u *User, p *Post, got, what string) bool {
	if p.captionUpdated.Load() {
		w.staleCaption.Add(1)
		w.log.Warn("キャプション変更後の読み取りが古い", "user", u.Username, "post", p.ID, "where", what, "got", got, "want", p.Caption)
		return false
	}
	return true
}

// profileChanging は直近 3 秒に表示名・自己紹介の変更があったか。その間は他の利用者が読んだ値が
// 新旧どちらでもあり得るので、プロフィールの照合をしない。
func (u *User) profileChanging() bool {
	return time.Since(time.Unix(0, u.profileChangedAt.Load())) < 3*time.Second
}
