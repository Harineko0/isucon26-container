package scenario

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"isureal-bench/internal/api"
	"isureal-bench/internal/client"
	"isureal-bench/internal/jpegs"
	"isureal-bench/internal/score"
)

// Result は公式ベンチの stdout 末尾の JSON ({"pass","score","messages"}) と同じ形に、内訳を足したもの。
type Result struct {
	Pass     bool     `json:"pass"`
	Score    int64    `json:"score"`
	Messages []string `json:"messages"`

	Reason        string                   `json:"reason,omitempty"`
	RawScore      int64                    `json:"rawScore"`
	Breakdown     []score.Line             `json:"breakdown"`
	ByRegion      map[string]float64       `json:"byRegion"`
	Requests      []client.EndpointSummary `json:"requests"`
	TotalRequests int64                    `json:"totalRequests"`
	ErrorCount    int64                    `json:"errorCount"`
	ErrorRate     float64                  `json:"errorRate"`
	InitializeSec float64                  `json:"initializeSec"`
	Language      string                   `json:"language"`
	LoadSec       float64                  `json:"loadSec"`
	Days          map[string][]int64       `json:"daysUsers"`
	DaysPosted    map[string][]int64       `json:"daysPosted"` // 日ごとの投稿数
	DaysPassed    map[string][]int64       `json:"daysPassed"` // 日ごとの友達フィード検証通過数
	GrandPrix     map[string]int64         `json:"grandPrixPassed"`
	Users         int                      `json:"users"`
	Posts         int                      `json:"posts"`
	// 友達フィードの検証で 1 回目に投稿が見えず再試行した回数 (read-after-write の遅れ)
	LateFeedVisibility int64 `json:"lateFeedVisibility"`
	// キャプション変更後の Memories・エクスポート・フィードで古いキャプションが返った回数
	StaleCaption int64 `json:"staleCaption"`
	// 表示名・自己紹介の変更後に、別の利用者のプロフィール取得が古い値を返した回数
	StaleProfile int64 `json:"staleProfile"`
}

type RunOptions struct {
	InitializeTimeout time.Duration
	SkipInitialize    bool
	SkipPrecheck      bool
	StartDate         string // manifest が無いときの最初の localDate (YYYY-MM-DD)
	AdminUsername     string
	AdminPassword     string
}

// Run は初期化 → 整合性チェック → 負荷 → 最終チェックを行う。
func (w *World) Run(ctx context.Context, opts RunOptions) *Result {
	res := &Result{Days: map[string][]int64{}, DaysPosted: map[string][]int64{}, DaysPassed: map[string][]int64{}, GrandPrix: map[string]int64{}}
	fail := func(reason string, err error) *Result {
		if err != nil {
			reason = fmt.Sprintf("%s: %v", reason, err)
		}
		w.log.Error(reason)
		res.Pass = false
		res.Reason = reason
		res.Messages = append([]string{reason}, w.Messages()...)
		w.fill(res)
		res.Score = 0
		return res
	}

	// 画像の用意
	rng := w.random()
	w.postImages = jpegs.NewPool(rng, w.cfg.ImagePoolSize, 1024, 768, w.cfg.PostImageBytes)
	w.realmojiImages = jpegs.NewPool(rng, w.cfg.ImagePoolSize/2+1, 320, 320, w.cfg.RealMojiBytes)
	w.profileImages = jpegs.NewPool(rng, w.cfg.ImagePoolSize/2+1, 256, 256, w.cfg.RealMojiBytes)

	// 1. 初期化
	if !opts.SkipInitialize {
		w.log.Info("初期化を実行します", "timeout", opts.InitializeTimeout)
		initCtx, cancel := context.WithTimeout(ctx, opts.InitializeTimeout)
		saved := w.client.Timeout
		w.client.Timeout = opts.InitializeTimeout
		start := time.Now()
		var init api.InitializeResponse
		_, err := w.client.JSON(initCtx, &client.Session{Country: "JP", ClientIP: w.newClientIP()}, http.MethodPost, "/api/initialize", "POST /api/initialize", nil, http.StatusOK, &init)
		w.client.Timeout = saved
		cancel()
		res.InitializeSec = time.Since(start).Seconds()
		if err != nil {
			return fail("初期化に失敗しました", err)
		}
		if strings.TrimSpace(init.Language) == "" {
			return fail("初期化の応答の language が空です", nil)
		}
		res.Language = init.Language
		w.log.Info("初期化が完了しました", "sec", fmt.Sprintf("%.2f", res.InitializeSec), "language", init.Language, "agent", init.Agent, "harness", init.Harness, "model", init.Model)
	}

	// 2. 準備 (管理者、初期データの利用者)
	if err := w.adminLogin(ctx, opts.AdminUsername, opts.AdminPassword); err != nil {
		return fail("管理者でログインできません", err)
	}
	if err := w.loadManifestUsers(ctx, opts.StartDate); err != nil {
		return fail("初期データの準備に失敗しました", err)
	}

	// 3. 整合性チェック
	if !opts.SkipPrecheck {
		w.log.Info("整合性チェックを実行します")
		w.precheck(ctx)
		if crit, reason := w.Critical(); crit {
			return fail("整合性チェックに失敗しました: "+reason, nil)
		}
	}

	// 4. 日 1 の利用者
	if err := w.prepareSeedUsers(ctx); err != nil {
		return fail("負荷走行用のユーザーを準備できません", err)
	}
	if crit, reason := w.Critical(); crit {
		return fail(reason, nil)
	}

	// 5. 負荷
	w.log.Info("負荷走行を開始します", "days", w.cfg.Days, "interval", w.cfg.WindowInterval, "grandprix", w.cfg.GrandPrix)
	loadCtx, cancelLoad := context.WithCancel(ctx)
	w.cancelLoad = cancelLoad
	w.loadCtx = loadCtx
	for _, u := range w.allActiveUsers() {
		if !u.LoggedOut() {
			w.startNotifier(u)
		}
	}
	loadStart := time.Now()
	firstOpen := loadStart.Add(time.Second)
	lastRegion := firstOpen.Add(time.Duration(len(api.Regions)-1) * w.cfg.Stagger)
	grandPrixAt := lastRegion.Add(time.Duration(w.cfg.Days) * w.cfg.WindowInterval)
	var wg sync.WaitGroup
	for i, region := range api.Regions {
		wg.Add(1)
		go w.runRegion(loadCtx, region, firstOpen.Add(time.Duration(i)*w.cfg.Stagger), grandPrixAt, &wg)
	}
	wg.Wait()
	w.loadEnded.Store(true)
	cancelLoad()
	res.LoadSec = time.Since(loadStart).Seconds()
	w.summarizeDays(res)
	if w.cfg.GrandPrix {
		w.log.Info(fmt.Sprintf("グランプリ: アジア %d 人, ヨーロッパ %d 人, アメリカ %d 人", res.GrandPrix["asia"], res.GrandPrix["europe"], res.GrandPrix["america"]))
	}

	// 6. 最終チェック
	w.finalCheck(ctx)

	w.fill(res)
	if crit, reason := w.Critical(); crit {
		res.Pass = false
		res.Reason = reason
		res.Score = 0
		res.Messages = append([]string{reason}, res.Messages...)
		return res
	}
	if res.ErrorRate > w.cfg.MaxErrorRate {
		res.Pass = false
		res.Reason = fmt.Sprintf("エラー率 %.1f%% が上限 %.1f%% を超えました", res.ErrorRate*100, w.cfg.MaxErrorRate*100)
		res.Score = 0
		res.Messages = append([]string{res.Reason}, res.Messages...)
		return res
	}
	res.Pass = true
	return res
}

func (w *World) fill(res *Result) {
	res.RawScore = w.board.Total()
	res.Score = res.RawScore
	res.Breakdown = w.board.Breakdown()
	res.ByRegion = w.board.ByRegion()
	res.Requests = w.client.Stats.Summary()
	res.TotalRequests, _ = w.client.Stats.Totals()
	res.ErrorCount = w.Errors()
	res.LateFeedVisibility = w.lateFeed.Load()
	res.StaleCaption = w.staleCaption.Load()
	res.StaleProfile = w.staleProfile.Load()
	if res.TotalRequests > 0 {
		res.ErrorRate = float64(res.ErrorCount) / float64(res.TotalRequests)
	}
	if res.Messages == nil {
		res.Messages = w.Messages()
	}
	w.usersMu.RLock()
	res.Users = len(w.users)
	w.usersMu.RUnlock()
	w.postsMu.RLock()
	res.Posts = len(w.posts)
	w.postsMu.RUnlock()
}

func (w *World) summarizeDays(res *Result) {
	type dw struct {
		day                   int
		users, posted, passed int64
	}
	byRegion := map[string][]dw{}
	w.windows.Range(func(_, v any) bool {
		win := v.(*Window)
		if win.Final {
			res.GrandPrix[win.Region] = win.passed.Load()
			res.DaysPosted[win.Region+"_final"] = []int64{win.posted.Load()}
		} else {
			byRegion[win.Region] = append(byRegion[win.Region], dw{win.Day, win.users.Load(), win.posted.Load(), win.passed.Load()})
		}
		return true
	})
	for r, list := range byRegion {
		sort.Slice(list, func(i, j int) bool { return list[i].day < list[j].day })
		for _, d := range list {
			res.Days[r] = append(res.Days[r], d.users)
			res.DaysPosted[r] = append(res.DaysPosted[r], d.posted)
			res.DaysPassed[r] = append(res.DaysPassed[r], d.passed)
		}
	}
}

// loadManifestUsers は manifest の利用者をベンチの世界に入れる (ログインはまだしない)。
func (w *World) loadManifestUsers(ctx context.Context, startDate string) error {
	for _, r := range api.Regions {
		rs := w.regions[r]
		if w.man != nil {
			if win, ok := w.man.Windows[r]; ok && win.LastLocalDate != "" {
				t, err := time.Parse("2006-01-02", win.LastLocalDate)
				if err != nil {
					return fmt.Errorf("manifest の lastLocalDate %q: %w", win.LastLocalDate, err)
				}
				rs.lastLocalDate = t
				continue
			}
		}
		t, err := time.Parse("2006-01-02", startDate)
		if err != nil {
			return fmt.Errorf("start-date %q: %w", startDate, err)
		}
		rs.lastLocalDate = t.AddDate(0, 0, -1)
	}
	if w.man == nil {
		return nil
	}
	byID := map[string]*User{}
	for _, mu := range w.man.Users {
		u := &User{
			ID: mu.ID, Username: mu.Username, SecretKey: mu.SecretKey, DisplayName: mu.DisplayName, Bio: mu.Bio, AccountType: mu.AccountType,
			ClientIP: w.newClientIP(), region: mu.Region, Initial: true,
			friends: map[string]*User{}, everFriends: map[string]bool{}, following: map[string]*User{}, blocked: map[string]time.Time{}, blockedBy: map[string]time.Time{},
			windowCounters: map[string]*score.WindowCounter{}, verifiedWindows: map[string]bool{},
		}
		for _, mp := range mu.Posts {
			p := &Post{ID: mp.ID, AuthorID: u.ID, LocalDate: mp.LocalDate, Region: mu.Region, Caption: mp.Caption, Visibility: mp.Visibility, PostedAt: mp.PostedAt}
			if sum, err := hex.DecodeString(mp.ImageSHA256); err == nil && len(sum) == 32 {
				copy(p.ImageSHA[:], sum)
				p.HasSHA = true
			}
			u.posts = append(u.posts, p)
			w.addPost(p)
		}
		byID[u.ID] = u
		w.addUser(u)
		if mu.AccountType == "certified_public" {
			rs := w.regions[mu.Region]
			rs.mu.Lock()
			rs.certified = append(rs.certified, u)
			rs.mu.Unlock()
		}
	}
	for _, mu := range w.man.Users {
		u := byID[mu.ID]
		for _, fid := range mu.Friends {
			if f := byID[fid]; f != nil {
				u.friends[f.ID] = f
				u.everFriends[f.ID] = true
			}
		}
		for _, fid := range mu.Follows {
			if f := byID[fid]; f != nil {
				u.following[f.ID] = f
			}
		}
		for _, bid := range mu.Blocks {
			if b := byID[bid]; b != nil {
				u.blocked[b.ID] = time.Time{}
				b.blockedBy[u.ID] = time.Time{}
			}
		}
	}
	w.log.Info("初期データの利用者を読み込みました", "users", len(w.man.Users))
	return nil
}

// precheck はアプリケーション整合性チェックと初期データの検証。
func (w *World) precheck(ctx context.Context) {
	rng := w.random()
	anon := &client.Session{Country: "JP", ClientIP: w.newClientIP()}
	// 認証なしは 401
	for _, path := range []string{"/api/me", "/api/feeds/friends", "/api/posting-window/current", "/api/notifications"} {
		if _, err := w.client.Do(ctx, anon, http.MethodGet, path, "GET "+path+" (401)", nil, "", http.StatusUnauthorized); err != nil {
			w.reportStatusMismatch(nil, "認証なしの "+path, err, http.StatusOK, "認証なしで "+path+" が応答しました")
		}
	}
	// 不正な国コードはルーターが 400 を返す (manual.md「通信経路」)
	bad := &client.Session{Country: "jp", ClientIP: anon.ClientIP}
	if _, err := w.client.Do(ctx, bad, http.MethodGet, "/api/me", "GET /api/me (bad country)", nil, "", http.StatusBadRequest); err != nil {
		w.report(false, nil, "小文字の X-Country-Code", err)
	}
	// 存在しないユーザーのログインは 401
	if _, err := w.client.JSON(ctx, anon, http.MethodPost, "/api/auth/login", "POST /api/auth/login (401)", map[string]string{"username": "no_such_user_isureal_bench", "secretKey": "x"}, http.StatusUnauthorized, nil); err != nil {
		w.reportStatusMismatch(nil, "存在しないユーザーのログイン", err, http.StatusOK, "存在しないユーザーでログインできました")
	}

	if w.man == nil {
		w.log.Info("manifest が無いので初期データの検証は行いません")
		return
	}
	// 初期データの利用者: ログイン・プロフィール・Memories・エクスポート・友達一覧
	var initial []*User
	for _, u := range w.allActiveUsers() {
		if u.Initial {
			initial = append(initial, u)
		}
	}
	rng.Shuffle(len(initial), func(i, j int) { initial[i], initial[j] = initial[j], initial[i] })
	n := min(len(initial), 30)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := 0; i < n; i++ {
		u := initial[i]
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := w.login(ctx, u, true); err != nil {
				return
			}
			if w.getMe(ctx, u) == nil {
				return
			}
			posts := u.Posts()
			if len(posts) > 0 {
				p := posts[rng.IntN(len(posts))]
				w.precheckMemories(ctx, u, p)
				if i < 4 {
					w.precheckExport(ctx, u)
				}
				if i < 10 {
					w.precheckMemoriesMonth(ctx, u, p.LocalDate[:7])
				}
			}
			if i < 10 {
				w.precheckFriends(ctx, u)
			}
		}(i)
	}
	wg.Wait()
	// 初期ユーザーの画像を数件
	for i := 0; i < min(n, 5); i++ {
		u := initial[i]
		if u.LoggedOut() {
			continue
		}
		for _, p := range u.Posts() {
			if p.ImageURL != "" {
				w.getImage(ctx, u, p.ImageURL, p, "", nil)
				break
			}
		}
	}
	w.log.Info("整合性チェックが完了しました", "checkedUsers", n, "errors", w.Errors())
}

func (w *World) precheckMemories(ctx context.Context, u *User, p *Post) {
	var res api.MemoriesResponse
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, "/api/memories/"+p.LocalDate, "GET /api/memories/:date", nil, http.StatusOK, &res)
	if err != nil {
		w.report(true, u, "初期データの Memories", err)
		return
	}
	for i := range res.Items {
		it := &res.Items[i]
		if it.ID == p.ID {
			if it.Caption != p.Caption || it.Visibility != p.Visibility {
				w.report(true, u, "初期データの Memories", fmt.Errorf("投稿 %s の内容が期待値と一致しません (caption=%q visibility=%q)", p.ID, it.Caption, it.Visibility))
				return
			}
			p.ImageURL = it.ImageURL
			p.WindowID = it.PostingWindowID
			// RealMoji・コメントの件数
			if mp := w.manifestPost(u, p.ID); mp != nil {
				if len(it.RealMojis) != min(mp.RealMojiCount, 50) || len(it.Comments) != min(mp.CommentCount, 50) {
					w.report(true, u, "初期データの Memories", fmt.Errorf("投稿 %s の RealMoji %d 件・コメント %d 件 (期待 %d・%d)", p.ID, len(it.RealMojis), len(it.Comments), mp.RealMojiCount, mp.CommentCount))
					return
				}
			}
			return
		}
	}
	w.report(true, u, "初期データの Memories", fmt.Errorf("%s の投稿 %s が初期データの期待値にあるのに Memories にありません", p.LocalDate, p.ID))
}

func (w *World) manifestPost(u *User, postID string) *manifestPostRef {
	if w.man == nil {
		return nil
	}
	for i := range w.man.Users {
		if w.man.Users[i].ID != u.ID {
			continue
		}
		for j := range w.man.Users[i].Posts {
			if w.man.Users[i].Posts[j].ID == postID {
				p := &w.man.Users[i].Posts[j]
				return &manifestPostRef{RealMojiCount: p.RealMojiCount, CommentCount: p.CommentCount}
			}
		}
	}
	return nil
}

type manifestPostRef struct{ RealMojiCount, CommentCount int }

func (w *World) precheckMemoriesMonth(ctx context.Context, u *User, month string) {
	var res api.MemoriesResponse
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, "/api/memories?month="+month, "GET /api/memories", nil, http.StatusOK, &res)
	if err != nil {
		w.report(true, u, "初期データの Memories (月)", err)
		return
	}
	expected := 0
	for _, p := range u.Posts() {
		if strings.HasPrefix(p.LocalDate, month) {
			expected++
		}
	}
	got := len(res.Items)
	if res.HasMore {
		if got != 50 || expected <= 50 {
			w.report(true, u, "初期データの Memories (月)", fmt.Errorf("%s の件数が合いません (hasMore で %d 件、期待 %d 件)", month, got, expected))
		}
		return
	}
	if got != expected {
		w.report(true, u, "初期データの Memories (月)", fmt.Errorf("%s の件数が %d 件 (期待 %d 件)", month, got, expected))
	}
	for i := range res.Items {
		if res.Items[i].ImageURL != "" {
			if p := w.post(res.Items[i].ID); p != nil {
				p.ImageURL = res.Items[i].ImageURL
			}
		}
	}
}

func (w *World) precheckExport(ctx context.Context, u *User) {
	before := w.Errors()
	if !w.export(ctx, u) && w.Errors() > before {
		// export は致命として記録済み (初期データの期待値と一致しない)
		return
	}
}

func (w *World) precheckFriends(ctx context.Context, u *User) {
	var res api.RelationshipsResponse
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, "/api/relationships/friends", "GET /api/relationships/:kind", nil, http.StatusOK, &res)
	if err != nil {
		w.report(true, u, "初期データの友達一覧", err)
		return
	}
	expected := len(u.Friends())
	if !res.HasMore && len(res.Items) != expected {
		w.report(true, u, "初期データの友達一覧", fmt.Errorf("友達が %d 人 (期待 %d 人)", len(res.Items), expected))
		return
	}
	for _, it := range res.Items {
		if !u.IsFriend(it.User.ID) {
			w.report(true, u, "初期データの友達一覧", fmt.Errorf("友達でない %s が含まれています", it.User.Username))
			return
		}
	}
}

// prepareSeedUsers は日 1 の利用者をそろえる。manifest に Seed 利用者がいればログインし、無ければ登録する。
func (w *World) prepareSeedUsers(ctx context.Context) error {
	rng := w.random()
	seeds := map[string][]*User{}
	if w.man != nil {
		for _, mu := range w.man.Users {
			if mu.Seed {
				if u := w.user(mu.ID); u != nil {
					seeds[mu.Region] = append(seeds[mu.Region], u)
				}
			}
		}
	}
	var wg sync.WaitGroup
	for _, r := range api.Regions {
		if len(seeds[r]) > 0 {
			for _, u := range seeds[r] {
				wg.Add(1)
				go func() {
					defer wg.Done()
					w.login(ctx, u, true)
				}()
			}
			continue
		}
		n := w.cfg.InitialUsers[r]
		wg.Add(1)
		go func() {
			defer wg.Done()
			var mu sync.Mutex
			var created []*User
			var inner sync.WaitGroup
			sem := make(chan struct{}, w.cfg.RegisterParallel)
			for i := 0; i < n; i++ {
				inner.Add(1)
				sem <- struct{}{}
				go func() {
					defer inner.Done()
					defer func() { <-sem }()
					if u, err := w.register(ctx, r, w.random()); err == nil {
						mu.Lock()
						created = append(created, u)
						mu.Unlock()
					}
				}()
			}
			inner.Wait()
			// 互いに友達になる (1 人あたり 3〜4 人) と、1 人を認定公開アカウントにする
			for i, u := range created {
				for k := 1; k <= 3 && len(created) > 1; k++ {
					t := created[(i+k)%len(created)]
					w.befriend(ctx, u, t)
				}
			}
			if len(created) > 0 {
				c := created[rng.IntN(len(created))]
				if err := w.certify(ctx, c); err != nil {
					w.report(false, c, "認定できません", err)
				} else {
					rs := w.regions[r]
					rs.mu.Lock()
					rs.certified = append(rs.certified, c)
					rs.mu.Unlock()
				}
			}
			mu.Lock()
			seeds[r] = created
			mu.Unlock()
		}()
	}
	wg.Wait()
	// 別リージョンの利用者とも友達に (リージョンをまたぐ read-after-write を検証するため)
	all := w.allActiveUsers()
	for _, u := range all {
		if u.LoggedOut() {
			continue
		}
		for i := 0; i < 1; i++ {
			t := all[rng.IntN(len(all))]
			if t.Region() != u.Region() && !t.LoggedOut() {
				w.befriend(ctx, u, t)
			}
		}
	}
	total := 0
	for _, r := range api.Regions {
		total += len(w.activeUsers(r))
	}
	if total == 0 {
		return fmt.Errorf("利用者がいません")
	}
	w.log.Info("負荷走行用のユーザーを準備しました", "asia", len(w.activeUsers("asia")), "europe", len(w.activeUsers("europe")), "america", len(w.activeUsers("america")))
	return nil
}

// finalCheck は負荷のあとに、削除した投稿の画像が 403 になっていることを確かめる。
func (w *World) finalCheck(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	checked := 0
	for _, u := range w.allActiveUsers() {
		if u.LoggedOut() {
			continue
		}
		u.mu.Lock()
		list := append([]deletedImage(nil), u.deletedImageURLs...)
		u.mu.Unlock()
		for _, d := range list {
			if time.Since(d.DeletedAt) < w.cfg.DeletionGrace {
				w.sleepUntil(ctx, d.DeletedAt.Add(w.cfg.DeletionGrace+200*time.Millisecond))
			}
			w.expectImageForbidden(ctx, u, d.URL, "削除した投稿の画像 (最終チェック)")
			checked++
			if checked >= 50 {
				return
			}
		}
	}
	_ = rand.New
}
