package scenario

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"isureal-bench/internal/api"
	"isureal-bench/internal/client"
	"isureal-bench/internal/score"
)

// isConflict は POST /api/admin/posting-window の 409 (posting_window_conflict) か。
func isConflict(err error) bool {
	var ce *client.Error
	return errors.As(err, &ce) && ce.Got == http.StatusConflict
}

// openWindowAuto は投稿期間を開く。manifest も -start-date も無い (rs.autoDate) 1 日目に 409 なら、初期データが
// もっと先の日付の投稿期間を持っているので、1, 2, 4, … 日ずつ先に進めて開ける日付を探す
// (409 は「その日以降の投稿期間がある」なので、通れば以後の日も通る)。開いた localDate を返す。
func (w *World) openWindowAuto(ctx context.Context, rs *regionState, region string, day int, localDate string) (*api.OpenPostingWindowResponse, string, error) {
	res, err := w.openWindow(ctx, region, localDate)
	if err == nil || day != 1 || !rs.autoDate || !isConflict(err) {
		return res, localDate, err
	}
	base, perr := time.Parse("2006-01-02", localDate)
	if perr != nil {
		return res, localDate, err
	}
	step := 1
	for try := 0; try < 12 && isConflict(err) && ctx.Err() == nil; try++ {
		base = base.AddDate(0, 0, step)
		localDate = base.Format("2006-01-02")
		rs.mu.Lock()
		rs.lastLocalDate = base // 以後の日はここから 1 日ずつ進む
		rs.mu.Unlock()
		step *= 2
		res, err = w.openWindow(ctx, region, localDate)
	}
	if err == nil {
		w.log.Info("投稿期間の開始日を先に進めました", "region", region, "first", localDate)
	}
	return res, localDate, err
}

// runRegion は 1 リージョンの投稿期間を順に開く。firstOpen は 1 日目の開始時刻。
// grandPrixAt が非ゼロなら、通常の日のあとその時刻に最終日を開く。
func (w *World) runRegion(ctx context.Context, region string, firstOpen, grandPrixAt time.Time, done *sync.WaitGroup) {
	defer done.Done()
	rs := w.regions[region]
	for day := 1; day <= w.cfg.Days; day++ {
		openAt := firstOpen.Add(time.Duration(day-1) * w.cfg.WindowInterval)
		endsAt := openAt.Add(w.cfg.WindowInterval)
		if day == w.cfg.Days && w.cfg.GrandPrix {
			endsAt = grandPrixAt
		}
		if !w.runDay(ctx, rs, region, day, false, openAt, endsAt) {
			return
		}
	}
	if w.cfg.GrandPrix {
		w.runDay(ctx, rs, region, w.cfg.Days+1, true, grandPrixAt, grandPrixAt.Add(w.cfg.GrandPrixDuration))
	}
}

// runDay は投稿期間を 1 つ開き、利用者の 1 日を走らせる。次の投稿期間 (endsAt) まで戻らない。
func (w *World) runDay(ctx context.Context, rs *regionState, region string, day int, final bool, openAt, endsAt time.Time) bool {
	rs.mu.Lock()
	rs.lastLocalDate = rs.lastLocalDate.AddDate(0, 0, 1)
	localDate := rs.lastLocalDate.Format("2006-01-02")
	prev := rs.current
	rs.mu.Unlock()

	win := &Window{Region: region, Day: day, Final: final, LocalDate: localDate, EndsAt: endsAt, ready: make(chan struct{})}
	users := w.activeUsers(region)
	win.users.Store(int64(len(users)))

	// 開く 1 秒前から利用者が通知を購読する
	if !w.sleepUntil(ctx, openAt.Add(-time.Second)) {
		return false
	}
	var wg sync.WaitGroup
	for _, u := range users {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.userDay(ctx, u, win)
		}()
	}
	if !w.sleepUntil(ctx, openAt) {
		close(win.ready)
		win.failed.Store(true)
		wg.Wait()
		return false
	}
	// 開く要求を出した時点から「前の投稿期間は終わりかけ」とみなす (サーバ側では応答より前に終わる)
	rs.mu.Lock()
	rs.current = win
	rs.day = day
	rs.mu.Unlock()
	res, localDate, err := w.openWindowAuto(ctx, rs, region, day, localDate)
	win.LocalDate = localDate
	if err != nil {
		win.failed.Store(true)
		close(win.ready)
		if ctx.Err() == nil {
			w.report(true, nil, fmt.Sprintf("%s の投稿期間を開けません", region), err)
		}
		wg.Wait()
		return false
	}
	win.ID = res.ID
	win.StartsAt = time.Now()
	win.Deadline = res.OnTimeDeadline
	w.windows.Store(win.ID, win)
	if prev != nil {
		prev.endedAt.Store(time.Now().UnixNano())
	}
	close(win.ready)
	if final {
		w.log.Info(fmt.Sprintf("%s 最終日（グランプリ）: %d 人", region, len(users)))
	} else {
		w.log.Info(fmt.Sprintf("%s %d 日目: %d 人", region, day, len(users)))
	}

	// 翌日の人数を決め、新しい利用者を登録する (最終日は増やさない)
	if !final {
		if w.sleepUntil(ctx, openAt.Add(w.cfg.GrowthSnapshot)) {
			passed := int(win.passed.Load())
			n := passed * w.cfg.GrowthFactor
			if w.cfg.MaxUsersPerRegion > 0 {
				if room := w.cfg.MaxUsersPerRegion - len(w.activeUsers(region)); n > room {
					n = max(room, 0)
				}
			}
			w.registerUsers(ctx, region, n, endsAt.Add(-500*time.Millisecond))
		}
	}
	w.sleepUntil(ctx, endsAt)
	wg.Wait()
	return ctx.Err() == nil
}

// registerUsers は n 人を登録し、既存の利用者と友達になる。deadline までに終わらない分は諦める。
func (w *World) registerUsers(ctx context.Context, region string, n int, deadline time.Time) {
	if n <= 0 {
		return
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	sem := make(chan struct{}, w.cfg.RegisterParallel)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			wg.Wait()
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			rng := w.random()
			u, err := w.register(ctx, region, rng)
			if err != nil {
				return
			}
			w.makeInitialFriends(ctx, u, rng)
		}()
	}
	wg.Wait()
}

// makeInitialFriends は新規利用者が同じリージョンと別リージョンの利用者に友達申請する。
// 公式の alp では登録 1 人あたり友達申請 約 5 件、承認 約 1.7 件、拒否・取り下げも少数。
func (w *World) makeInitialFriends(ctx context.Context, u *User, rng *rand.Rand) {
	same := w.activeUsers(u.Region())
	var others []*User
	for _, r := range api.Regions {
		if r != u.Region() {
			others = append(others, w.activeUsers(r)...)
		}
	}
	pick := func(pool []*User) *User {
		for i := 0; i < 8 && len(pool) > 0; i++ {
			t := pool[rng.IntN(len(pool))]
			if t.ID != u.ID && !t.Deleted() && !u.IsFriend(t.ID) {
				return t
			}
		}
		return nil
	}
	// 承認される申請: 同リージョン 2 + 別リージョン 1
	for i := 0; i < 3; i++ {
		pool := same
		if i == 2 && len(others) > 0 {
			pool = others
		}
		if t := pick(pool); t != nil {
			w.befriend(ctx, u, t)
		}
	}
	// 拒否される申請
	if t := pick(same); t != nil {
		if id, accepted := w.friendRequest(ctx, u, t); id != "" && !accepted && w.ensureLogin(ctx, t) {
			w.transitionRequest(ctx, t, id, "reject")
		}
	}
	// 取り下げる申請
	if t := pick(append(same, others...)); t != nil {
		if id, accepted := w.friendRequest(ctx, u, t); id != "" && !accepted {
			w.transitionRequest(ctx, u, id, "cancel")
		}
	}
	// 認定公開アカウントをフォロー
	if cert := w.pickCertified(rng); cert != nil && cert.ID != u.ID {
		w.follow(ctx, u, cert)
	}
}

func (w *World) pickCertified(rng *rand.Rand) *User {
	var all []*User
	for _, rs := range w.regions {
		rs.mu.Lock()
		all = append(all, rs.certified...)
		rs.mu.Unlock()
	}
	for i := 0; i < 4 && len(all) > 0; i++ {
		c := all[rng.IntN(len(all))]
		if !c.Deleted() {
			return c
		}
	}
	return nil
}

// userDay は 1 利用者の 1 投稿期間ぶんの行動。
func (w *World) userDay(ctx context.Context, u *User, win *Window) {
	// 前の日の行動 (ログアウトなど) が終わるのを待つ。投稿期間が終わるまで空かなければ諦める
	for !u.busy.CompareAndSwap(0, 1) {
		if ctx.Err() != nil || time.Now().After(win.EndsAt) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer u.busy.Store(0)
	rng := w.random()
	dayCtx, cancel := context.WithDeadline(ctx, win.EndsAt)
	defer cancel()
	if u.Deleted() || !w.ensureLogin(dayCtx, u) {
		return
	}
	counter := u.counter(win.ID)

	// 通知を購読して投稿期間が開くのを待つ
	notified := w.waitForWindow(dayCtx, u, win)
	if win.failed.Load() || dayCtx.Err() != nil {
		return
	}
	if u.Region() != win.Region {
		return // 移動した
	}
	counter = u.counter(win.ID)
	if notified {
		w.board.Add(score.Notification, u.Region())
	}

	// 投稿期間の状態を確かめる
	cur, err := w.currentWindow(dayCtx, u)
	if err != nil {
		return
	}
	if cur.Window == nil || cur.Window.ID != win.ID {
		got := "null"
		if cur.Window != nil {
			got = cur.Window.ID + " (" + cur.Window.LocalDate + ")"
		}
		w.report(true, u, "投稿期間", fmt.Errorf("開いている投稿期間が %s (期待 %s %s)", got, win.ID, win.LocalDate))
		return
	}
	if cur.HasPosted || !cur.CanPost {
		w.report(true, u, "投稿期間", fmt.Errorf("投稿前なのに hasPosted=%v canPost=%v", cur.HasPosted, cur.CanPost))
		return
	}

	// 投稿と直後の確認
	post, err := w.createPost(dayCtx, u, win, rng)
	if err != nil {
		return
	}
	if !w.myCurrentPost(dayCtx, u, post) {
		return
	}
	if !w.getImage(dayCtx, u, post.ImageURL, post, score.ImageGet, nil) {
		return
	}
	// 署名の検証 (改竄・期限切れ) をときどき
	if w.chance(rng, w.cfg.NegativeCheckRate) {
		if tampered, expired := tamperSignature(post.ImageURL); tampered != "" {
			w.expectImageForbidden(dayCtx, u, tampered, "画像署名の検証")
			w.expectImageForbidden(dayCtx, u, expired, "画像の期限の検証")
		}
	}

	// 友達のフィードで検証 (投稿期間ごとに 1 回)
	w.verifyInFriendsFeeds(dayCtx, u, post, win)

	// 前回の投稿を Memories で確認
	w.checkPreviousMemory(dayCtx, u, win, rng)

	// 投稿を削除して 5 秒後に画像が 403 になることを確かめ、投稿し直す
	if w.chance(rng, w.cfg.DeletePostRate) && time.Until(win.EndsAt) > w.cfg.DeletionGrace+2*time.Second {
		if w.deletePost(dayCtx, u, post) {
			deletedURL := post.ImageURL
			if w.sleepUntil(dayCtx, time.Now().Add(w.cfg.DeletionGrace+500*time.Millisecond)) {
				w.expectImageForbidden(dayCtx, u, deletedURL, "削除した投稿の画像")
			}
			if p2, err := w.createPost(dayCtx, u, win, rng); err == nil {
				post = p2
				w.myCurrentPost(dayCtx, u, post)
			}
		}
	}

	// 閲覧と交流
	w.browse(dayCtx, u, win, post, counter, rng)

	// 1 日の終わり
	if ctx.Err() != nil {
		return
	}
	end, cancelEnd := context.WithTimeout(ctx, 3*time.Second)
	defer cancelEnd()
	switch {
	case w.chance(rng, w.cfg.DeleteAccountRate) && !u.Initial:
		w.deleteAccount(end, u)
	case w.chance(rng, w.cfg.MoveRate):
		w.changeRegion(end, u, rng)
	case w.chance(rng, w.cfg.LogoutRate):
		w.logout(end, u)
	}
}

// startNotifier は利用者ごとに 1 本、GET /api/notifications (SSE) を張り続ける goroutine を起こす。
// 参照実装は接続より後に作られた通知しか流さないので、投稿期間が開くより前に接続しておく必要がある。
// 公式の alp でも SSE の接続は平均 27 秒・最大 61 秒生きていた (投稿期間の 9 秒より長い)。
func (w *World) startNotifier(u *User) {
	if w.loadCtx == nil {
		return
	}
	u.notifierOnce.Do(func() {
		u.notifyCh = make(chan *api.Notification, 1)
		go func() {
			ctx := w.loadCtx
			for ctx.Err() == nil && !u.Deleted() {
				sess := u.Session()
				if sess == nil {
					time.Sleep(200 * time.Millisecond)
					continue
				}
				n, err := w.client.Notification(ctx, sess)
				if err != nil {
					// 退会・移動・ログアウトの直後に接続が閉じるのは仕様 (参照実装は利用者が居なければ閉じる)。
					// 接続している間にセッションが変わった (ログアウト・再ログイン) ときの 401 も数えない
					if !u.Deleted() && !u.sessionJustChanged() && u.Session() == sess {
						w.report(false, u, "通知を購読できません", err)
					}
					if !w.sleepUntil(ctx, time.Now().Add(500*time.Millisecond)) {
						return
					}
					continue
				}
				// 古い通知が残っていれば捨てて、新しいものを入れる
				select {
				case u.notifyCh <- n:
				default:
					select {
					case <-u.notifyCh:
					default:
					}
					u.notifyCh <- n
				}
			}
		}()
	})
}

// waitForWindow は投稿期間が開くのを待ち、その通知が届くのを待つ。戻り値は通知を受け取れたか。
// 開いてから 3 秒以内に届かなければエラーとして数え、投稿期間の API で確かめて進む。
func (w *World) waitForWindow(ctx context.Context, u *User, win *Window) bool {
	w.startNotifier(u)
	select {
	case <-win.ready:
	case <-ctx.Done():
		return false
	}
	if win.failed.Load() {
		return false
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case n := <-u.notifyCh:
			if n.PostingWindowID == win.ID {
				if n.Region != win.Region || n.ID == "" {
					w.report(true, u, "通知", fmt.Errorf("通知の内容が一致しません (window=%q region=%q)", n.PostingWindowID, n.Region))
					return false
				}
				return true
			}
			if old := w.window(n.PostingWindowID); old != nil && old.Ended() {
				continue // 前の投稿期間の通知が遅れて届いた
			}
			w.report(true, u, "通知", fmt.Errorf("開いている投稿期間 %s と違う通知 (window=%q region=%q)", win.ID, n.PostingWindowID, n.Region))
			return false
		case <-timer.C:
			w.report(false, u, "通知", fmt.Errorf("投稿期間が開いてから 3 秒以内に通知が届きません"))
			return false
		case <-ctx.Done():
			return false
		}
	}
}

// verifyInFriendsFeeds は、今日すでに投稿している友達のフィードに自分の投稿が載ることを確かめる。
// 友達がまだ投稿していなければ少し待って再試行する (投稿期間が終わるまで)。
func (w *World) verifyInFriendsFeeds(ctx context.Context, u *User, post *Post, win *Window) {
	rng := w.random()
	kind := score.FeedVerify
	if win.Final {
		kind = score.FeedVerifyFinal
	}
	misses := 0
	for attempt := 0; ctx.Err() == nil; attempt++ {
		if win.Ended() || time.Until(win.EndsAt) < 300*time.Millisecond {
			return // 投稿期間が終わった。加点は無い
		}
		var viewer *User
		var viewerWin *Window
		friends := u.Friends()
		rng.Shuffle(len(friends), func(i, j int) { friends[i], friends[j] = friends[j], friends[i] })
		for _, f := range friends {
			if f.Deleted() || f.LoggedOut() || u.IsBlockedEither(f.ID) {
				continue
			}
			// 閲覧者は自分のリージョンの開いている投稿期間に投稿済みで、その期間がすぐには終わらないこと
			if fw := w.currentWindowOf(f); fw != nil && !fw.Ended() && time.Until(fw.EndsAt) > time.Second && f.PostInWindow(fw.ID) != nil {
				viewer, viewerWin = f, fw
				break
			}
		}
		if viewer == nil {
			if len(friends) == 0 {
				return
			}
			if !w.sleepUntil(ctx, time.Now().Add(300*time.Millisecond)) {
				return
			}
			continue
		}
		found, err := w.findInFeed(ctx, viewer, post, 3, viewer.counter(viewerWin.ID))
		if err != nil {
			return
		}
		if !found {
			// 読んでいる間に状況が変わっていれば判定しない
			if win.Ended() || post.Deleted() || viewerWin.Ended() || viewer.LoggedOut() || viewer.Deleted() || u.IsBlockedEither(viewer.ID) || !u.IsFriend(viewer.ID) {
				continue
			}
			// 書き込み直後の読み取りは即反映が仕様 (manual.md「データの反映」)。公式がどこまで許すかは
			// 分からないので、短い再試行 (3 回・約 1 秒) の後も見えなければ致命にする。再試行で見えた
			// 件数は result.json の lateFeedVisibility に出す。
			misses++
			w.lateFeed.Add(1)
			if misses < 3 {
				w.log.Warn("投稿がまだ友達のフィードに無い。再試行する", "user", u.Username, "viewer", viewer.Username, "post", post.ID, "attempt", misses)
				if !w.sleepUntil(ctx, time.Now().Add(300*time.Millisecond)) {
					return
				}
				continue
			}
			w.report(true, u, "友達フィードの検証", fmt.Errorf("投稿 %s が友達 %s (%s) の friends フィードに %d 回読んでもありません", post.ID, viewer.Username, viewer.Region(), misses))
			return
		}
		u.mu.Lock()
		first := !u.verifiedWindows[win.ID]
		u.verifiedWindows[win.ID] = true
		u.passedDays++
		u.mu.Unlock()
		if first {
			win.passed.Add(1)
			w.board.Add(kind, u.Region())
		}
		return
	}
}

func (w *World) currentWindowOf(u *User) *Window {
	rs := w.regions[u.Region()]
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.current
}

func (w *World) currentWindowID(u *User) string {
	rs := w.regions[u.Region()]
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.current == nil {
		return ""
	}
	return rs.current.ID
}

func (w *World) checkPreviousMemory(ctx context.Context, u *User, win *Window, rng *rand.Rand) {
	posts := u.Posts()
	var prev *Post
	for i := len(posts) - 1; i >= 0; i-- {
		if posts[i].WindowID != win.ID && !posts[i].Deleted() {
			prev = posts[i]
			break
		}
	}
	if prev == nil {
		return
	}
	w.memoriesDate(ctx, u, prev.LocalDate, prev, score.MemoryPrevious)
}

// browse は投稿期間が終わるまで閲覧・交流を続ける。
func (w *World) browse(ctx context.Context, u *User, win *Window, own *Post, counter *score.WindowCounter, rng *rand.Rand) {
	// 公式の alp (asia1、利用者・日あたり): feeds/friends 1.3、images 6.3、memories 1.6、users 検索 1.0、
	// friend-requests 1.9、posting-window 1.3、fof 0.4、public 0.4、following 0.2、export 0.15、
	// profile-image 0.25、PATCH me 0.18、follows 0.23、incoming-requests 0.25、posts/:id 0.3 (EU)。
	type action struct {
		weight int
		run    func()
	}
	actions := []action{
		{12, func() {
			if _, err := w.feed(ctx, u, "friends", nil, counter); err == nil && w.chance(rng, 0.3) {
				w.feedPages(ctx, u, "friends", counter, rng)
			}
		}},
		{5, func() { w.feed(ctx, u, "friends-of-friends", nil, counter) }},
		{5, func() { w.feed(ctx, u, "public", nil, counter) }},
		{3, func() {
			if len(u.Following()) > 0 {
				w.feed(ctx, u, "following", nil, counter)
			}
		}},
		{60, func() {
			if p := u.pickSeen(rng); p != nil {
				w.getImage(ctx, u, p.ImageURL, w.post(p.ID), score.ImageGet, counter)
			}
		}},
		{6, func() {
			if p := u.pickSeen(rng); p != nil {
				w.getPost(ctx, u, p, counter)
			}
		}},
		{3, func() {
			if p := u.pickSeen(rng); p != nil {
				w.listRealMojis(ctx, u, p, counter)
			}
		}},
		{3, func() {
			// コメント一覧はコメント権限 (友達か public、かつ今日投稿済み) が要る
			if p := u.pickSeen(rng); p != nil && (p.Visibility == "public" || u.IsFriend(p.AuthorID)) {
				w.listComments(ctx, u, p, counter)
			}
		}},
		{6, func() {
			if p := u.pickSeen(rng); p != nil {
				if a := w.user(p.AuthorID); a != nil {
					w.getUser(ctx, u, a, counter)
				}
			}
		}},
		{2, func() {
			if p := u.pickSeen(rng); p != nil && counter.Remaining(score.RealMojiSend) > 0 {
				w.sendRealMoji(ctx, u, p, rng, counter)
			}
		}},
		{2, func() {
			if p := u.pickSeen(rng); p != nil && counter.Remaining(score.CommentPost) > 0 && (p.Visibility == "public" || u.IsFriend(p.AuthorID)) {
				w.sendComment(ctx, u, p, rng, counter)
			}
		}},
		{10, func() {
			if t := w.randomUser(rng, u); t != nil {
				w.searchUsers(ctx, u, t)
			}
		}},
		{8, func() {
			if t := w.randomUser(rng, u); t != nil && !u.IsFriend(t.ID) {
				if w.chance(rng, 0.3) && w.ensureLogin(ctx, t) {
					w.listRelationships(ctx, t, "incoming-requests")
				}
				w.befriend(ctx, u, t)
			}
		}},
		{2, func() {
			kinds := []string{"friends", "following", "followers"}
			w.listRelationships(ctx, u, kinds[rng.IntN(len(kinds))])
		}},
		{1, func() {
			if c := w.pickCertified(rng); c != nil && c.ID != u.ID {
				if u.IsFollowing(c.ID) {
					w.unfollow(ctx, u, c)
				} else {
					w.follow(ctx, u, c)
				}
			}
		}},
		{6, func() {
			posts := u.Posts()
			if len(posts) > 0 {
				p := posts[rng.IntN(len(posts))]
				if !p.Deleted() {
					if rng.IntN(2) == 0 {
						w.memoriesMonth(ctx, u, p.LocalDate[:7])
					} else {
						w.memoriesDate(ctx, u, p.LocalDate, p, score.MemoriesDate)
					}
				}
			}
		}},
		{4, func() {
			// 過去の投稿の画像
			posts := u.Posts()
			if len(posts) > 1 {
				p := posts[rng.IntN(len(posts)-1)]
				if !p.Deleted() && p.ImageURL != "" && p.WindowID != win.ID {
					w.getImage(ctx, u, p.ImageURL, p, score.PastImageGet, nil)
				}
			}
		}},
		{2, func() { w.export(ctx, u) }},
		{2, func() { w.updateProfile(ctx, u, rng) }},
		{2, func() { w.putProfileImage(ctx, u, rng) }},
		{2, func() {
			if own != nil && !own.Deleted() {
				w.updatePost(ctx, u, own, rng)
			}
		}},
		{1, func() {
			if t := w.randomUser(rng, u); t != nil && !u.IsBlockedEither(t.ID) && !t.Initial {
				if w.block(ctx, u, t) {
					if w.ensureLogin(ctx, t) {
						w.expectUserHidden(ctx, t, u)
					}
					if w.chance(rng, 0.5) {
						w.unblock(ctx, u, t)
					}
				}
			}
		}},
		{2, func() {
			// 閲覧権限のない投稿が見えないこと: 友達でない人の friends 限定投稿
			if p := w.randomHiddenPost(rng, u); p != nil {
				w.expectPostHidden(ctx, u, p)
			}
		}},
	}
	total := 0
	for _, a := range actions {
		total += a.weight
	}
	// 投稿期間の間 (閲覧の上限に当たっても検索や友達申請は続く)
	for ctx.Err() == nil && time.Until(win.EndsAt) > 200*time.Millisecond {
		if !w.think(ctx, rng) {
			return
		}
		r := rng.IntN(total)
		for _, a := range actions {
			if r < a.weight {
				a.run()
				break
			}
			r -= a.weight
		}
	}
}

// feedPages は 2 ページ目以降を 1〜2 ページ読む。
func (w *World) feedPages(ctx context.Context, u *User, kind string, counter *score.WindowCounter, rng *rand.Rand) {
	page, err := w.feed(ctx, u, kind, nil, counter)
	if err != nil || !page.hasMore || len(page.items) == 0 {
		return
	}
	last := &page.items[len(page.items)-1]
	w.feed(ctx, u, kind, last, counter)
}

func (w *World) randomUser(rng *rand.Rand, except *User) *User {
	region := except.Region()
	if rng.IntN(4) == 0 {
		region = api.Regions[rng.IntN(len(api.Regions))]
	}
	users := w.activeUsers(region)
	for i := 0; i < 4 && len(users) > 0; i++ {
		t := users[rng.IntN(len(users))]
		if t.ID != except.ID && !t.Deleted() {
			return t
		}
	}
	return nil
}

// randomHiddenPost は u が見てはいけない (友達でない人の friends 限定) 投稿を探す。
func (w *World) randomHiddenPost(rng *rand.Rand, u *User) *Post {
	for i := 0; i < 6; i++ {
		t := w.randomUser(rng, u)
		if t == nil || u.EverFriend(t.ID) {
			continue
		}
		for _, p := range t.Posts() {
			if p.Visibility == "friends" && !p.Deleted() {
				return p
			}
		}
	}
	return nil
}

func (w *World) expectUserHidden(ctx context.Context, viewer, target *User) {
	_, err := w.client.Do(ctx, viewer.Session(), http.MethodGet, "/api/users/"+target.ID, "GET /api/users/:id (404)", nil, "", http.StatusNotFound)
	if err != nil {
		w.reportStatusMismatch(viewer, "ブロック後のプロフィール", err, http.StatusOK, "ブロックした相手からプロフィールが見えています")
	}
}
