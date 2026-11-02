package scenario

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"isureal-bench/internal/api"
	"isureal-bench/internal/client"
	"isureal-bench/internal/score"
)

// ---- 認証 ----

func (w *World) register(ctx context.Context, region string, rng *rand.Rand) (*User, error) {
	u := &User{
		Username: w.newUsername(rng), DisplayName: displayNames[rng.IntN(len(displayNames))], AccountType: "regular",
		ClientIP: w.newClientIP(), region: region,
		friends: map[string]*User{}, everFriends: map[string]bool{}, following: map[string]*User{}, blocked: map[string]time.Time{}, blockedBy: map[string]time.Time{},
		windowCounters: map[string]*score.WindowCounter{}, verifiedWindows: map[string]bool{},
	}
	anon := &client.Session{Country: countryFor(region, u.Username), ClientIP: u.ClientIP}
	var res api.RegisterResponse
	_, err := w.client.JSON(ctx, anon, http.MethodPost, "/api/auth/register", "POST /api/auth/register",
		map[string]string{"username": u.Username, "displayName": u.DisplayName, "region": region}, http.StatusCreated, &res)
	if err != nil {
		w.report(false, u, "ユーザー登録できません", err)
		return nil, err
	}
	if res.SecretKey == "" || res.User.ID == "" || res.User.Username != u.Username || res.User.Region != region {
		err := fmt.Errorf("登録の応答が一致しません (secretKey=%v id=%q username=%q region=%q)", res.SecretKey != "", res.User.ID, res.User.Username, res.User.Region)
		w.report(true, u, "ユーザー登録の応答", err)
		return nil, err
	}
	u.ID = res.User.ID
	u.SecretKey = res.SecretKey
	if err := w.login(ctx, u, false); err != nil {
		return nil, err
	}
	w.addUser(u)
	w.startNotifier(u)
	return u, nil
}

// login はログインしてセッションを持つ。initial なら 401 は致命 (初期データのユーザーのログインが 401)。
func (w *World) login(ctx context.Context, u *User, initial bool) error {
	anon := &client.Session{Country: countryFor(u.Region(), u.ID), ClientIP: u.ClientIP}
	var res api.LoginResponse
	_, err := w.client.JSON(ctx, anon, http.MethodPost, "/api/auth/login", "POST /api/auth/login",
		map[string]string{"username": u.Username, "secretKey": u.SecretKey}, http.StatusOK, &res)
	if err != nil {
		var e *client.Error
		critical := initial && errors.As(err, &e) && e.Got == http.StatusUnauthorized
		w.report(critical, u, "ログインできません", err)
		return err
	}
	if res.Token == "" || res.User.Username != u.Username {
		err := fmt.Errorf("ログインの応答が一致しません")
		w.report(true, u, "ログインの応答", err)
		return err
	}
	if u.ID == "" {
		u.ID = res.User.ID
	}
	u.setSession(res.Token)
	return nil
}

func (w *World) ensureLogin(ctx context.Context, u *User) bool {
	if !u.LoggedOut() {
		return true
	}
	// 同時に 2 回ログインすると先のトークンが失効する (replaced) ので直列にする
	u.loginMu.Lock()
	defer u.loginMu.Unlock()
	if !u.LoggedOut() {
		return true
	}
	if u.Deleted() {
		return false
	}
	return w.login(ctx, u, false) == nil
}

func (w *World) logout(ctx context.Context, u *User) {
	sess := u.Session()
	if sess == nil {
		return
	}
	u.setSession("")
	u.lastLogin.Store(time.Now().UnixNano())
	if _, err := w.client.Do(ctx, sess, http.MethodPost, "/api/auth/logout", "POST /api/auth/logout", nil, "", http.StatusNoContent); err != nil {
		w.report(false, u, "ログアウトできません", err)
	}
}

func (w *World) adminLogin(ctx context.Context, username, password string) error {
	anon := &client.Session{Country: "JP", ClientIP: w.newClientIP()}
	var res api.AdminLoginResponse
	if _, err := w.client.JSON(ctx, anon, http.MethodPost, "/api/admin/auth/login", "POST /api/admin/auth/login",
		map[string]string{"username": username, "password": password}, http.StatusOK, &res); err != nil {
		return err
	}
	if res.Token == "" {
		return fmt.Errorf("管理者のトークンが空です")
	}
	w.admin = &client.Session{Token: res.Token, Country: "JP", ClientIP: anon.ClientIP}
	return nil
}

func (w *World) openWindow(ctx context.Context, region, localDate string) (*api.OpenPostingWindowResponse, error) {
	var res api.OpenPostingWindowResponse
	_, err := w.client.JSON(ctx, w.admin, http.MethodPost, "/api/admin/posting-window", "POST /api/admin/posting-window",
		map[string]any{"region": region, "localDate": localDate, "onTimeSeconds": w.cfg.OnTimeSeconds}, http.StatusCreated, &res)
	if err != nil {
		return nil, err
	}
	if res.Region != region || res.LocalDate != localDate || res.ID == "" {
		return nil, fmt.Errorf("投稿期間の応答が一致しません (%+v)", res)
	}
	return &res, nil
}

func (w *World) certify(ctx context.Context, u *User) error {
	_, err := w.client.Do(ctx, w.admin, http.MethodPut, "/api/admin/users/"+u.ID+"/certification", "PUT /api/admin/users/:id/certification", nil, "", http.StatusNoContent)
	if err == nil {
		u.mu.Lock()
		u.AccountType = "certified_public"
		u.mu.Unlock()
	}
	return err
}

// ---- 投稿 ----

var captions = []string{"今日の椅子", "ランチ", "散歩", "World Grand Prix entry", "my chair 🪑", "Bonjour", "Guten Morgen", "coffee break", "", "雨"}

func (w *World) currentWindow(ctx context.Context, u *User) (*api.CurrentPostingWindowResponse, error) {
	var res api.CurrentPostingWindowResponse
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, "/api/posting-window/current", "GET /api/posting-window/current", nil, http.StatusOK, &res)
	if err != nil {
		w.report(false, u, "投稿期間を取得できません", err)
		return nil, err
	}
	return &res, nil
}

func (w *World) createPost(ctx context.Context, u *User, win *Window, rng *rand.Rand) (*Post, error) {
	img := w.postImages.Pick(rng)
	visibility := []string{"friends", "friends", "friends", "friends_of_friends", "public"}[rng.IntN(5)]
	fields := map[string]string{"caption": captions[rng.IntN(len(captions))], "visibility": visibility}
	switch rng.IntN(3) {
	case 0:
		fields["location_kind"] = "none"
	case 1:
		fields["location_kind"] = "exact"
		fields["latitude"] = fmt.Sprintf("%.4f", rng.Float64()*180-90)
		fields["longitude"] = fmt.Sprintf("%.4f", rng.Float64()*360-180)
	default:
		fields["location_kind"] = "approximate"
		fields["locality"] = []string{"Tokyo", "Paris", "Berlin", "New York", "大阪", "Sydney"}[rng.IntN(6)]
	}
	body, ct, err := client.Multipart(fields, client.FilePart{Field: "image", Filename: "photo.jpg", Data: img.Data})
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := w.client.Do(ctx, u.Session(), http.MethodPost, "/api/posts", "POST /api/posts", body, ct, http.StatusCreated)
	if err != nil {
		var e *client.Error
		if !errors.As(err, &e) || e.Got == 0 || e.Got >= 500 {
			// 応答が無い (タイムアウト・切断) か 5xx。サーバ側では投稿が作られたかもしれない
			u.uncertainPosts.Store(true)
		}
		if errors.As(err, &e) && e.Got == http.StatusConflict && (win.Ended() || time.Now().After(win.EndsAt)) {
			// 期間が終わった後の 409 は仕様どおり
			return nil, err
		}
		w.report(false, u, "投稿できません", err)
		return nil, err
	}
	var res api.Post
	if err := json.Unmarshal(resp.Body, &res); err != nil {
		w.report(false, u, "投稿の応答", fmt.Errorf("JSON を解釈できません: %w", err))
		return nil, err
	}
	if res.ID == "" || res.Author.ID != u.ID || res.ImageURL == "" || res.Visibility != visibility || res.Caption != fields["caption"] || res.Location.Kind != fields["location_kind"] {
		err := fmt.Errorf("投稿の応答が要求と一致しません (id=%q author=%q visibility=%q caption=%q location=%q)", res.ID, res.Author.ID, res.Visibility, res.Caption, res.Location.Kind)
		w.report(true, u, "投稿の応答", err)
		return nil, err
	}
	if res.PostingWindowID != win.ID {
		err := fmt.Errorf("投稿の postingWindowId が開いている投稿期間と違います (%q != %q)", res.PostingWindowID, win.ID)
		w.report(true, u, "投稿の応答", err)
		return nil, err
	}
	// onTime / lateSeconds: 期限は投稿期間の応答から分かる。時計の誤差を 1 秒見て検査する
	late := res.PostedAt.Sub(win.Deadline)
	if res.OnTime && late > time.Second {
		w.report(true, u, "投稿の応答", fmt.Errorf("期限を %v 過ぎた投稿が onTime=true", late))
	} else if !res.OnTime && late < -time.Second {
		w.report(true, u, "投稿の応答", fmt.Errorf("期限より %v 前の投稿が onTime=false", -late))
	} else if !res.OnTime && res.LateSeconds != int64(late/time.Second) && res.LateSeconds != int64(late/time.Second)+1 && res.LateSeconds != int64(late/time.Second)-1 {
		w.report(true, u, "投稿の応答", fmt.Errorf("lateSeconds=%d が経過 %v と合いません", res.LateSeconds, late))
	} else if res.OnTime && res.LateSeconds != 0 {
		w.report(true, u, "投稿の応答", fmt.Errorf("onTime なのに lateSeconds=%d", res.LateSeconds))
	}
	_ = start
	p := &Post{ID: res.ID, AuthorID: u.ID, WindowID: win.ID, LocalDate: win.LocalDate, Region: win.Region, ImageURL: res.ImageURL, ImageSHA: img.SHA256, HasSHA: true, Caption: res.Caption, Visibility: visibility, PostedAt: res.PostedAt}
	w.addPost(p)
	u.addPost(p)
	win.posted.Add(1)
	return p, nil
}

func (w *World) myCurrentPost(ctx context.Context, u *User, expect *Post) bool {
	var res api.Post
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, "/api/posts/me/current", "GET /api/posts/me/current", nil, http.StatusOK, &res)
	if err != nil {
		w.report(false, u, "自分の投稿を取得できません", err)
		return false
	}
	if res.ID != expect.ID || res.Author.ID != u.ID || res.Caption != expect.Caption || res.Visibility != expect.Visibility {
		w.report(true, u, "自分の投稿", fmt.Errorf("投稿直後の自分の投稿が一致しません (id=%q caption=%q)", res.ID, res.Caption))
		return false
	}
	w.board.Add(score.PostGet, u.Region())
	return true
}

// getImage は署名付き URL の画像を取る。expect があれば内容を照合する。kind が空なら加点しない。
func (w *World) getImage(ctx context.Context, u *User, imageURL string, expect *Post, kind score.Kind, counter *score.WindowCounter) bool {
	resp, err := w.client.Do(ctx, u.Session(), http.MethodGet, imageURL, "GET /api/images/:id", nil, "", http.StatusOK)
	if err != nil {
		var e *client.Error
		if errors.As(err, &e) && e.Got == http.StatusForbidden && expect != nil {
			// 投稿が削除された・投稿者が退会した画像は 403 でよい
			if expect.Deleted() {
				return false
			}
			if author := w.user(expect.AuthorID); author != nil && author.Deleted() {
				return false
			}
		}
		w.report(false, u, "画像を取得できません", err)
		return false
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/") {
		w.report(true, u, "画像の取得", fmt.Errorf("Content-Type が画像ではありません: %q", ct))
		return false
	}
	if len(resp.Body) == 0 {
		w.report(true, u, "画像の取得", fmt.Errorf("本文が空です"))
		return false
	}
	if expect != nil && expect.HasSHA {
		if sha256.Sum256(resp.Body) != expect.ImageSHA {
			w.report(true, u, "画像の取得", fmt.Errorf("画像の内容が投稿したものと一致しません (%s)", imageURL))
			return false
		}
	}
	if kind != "" && (counter == nil || counter.Take(kind)) {
		w.board.Add(kind, u.Region())
	}
	return true
}

// expectImageForbidden は 403 を期待する (署名改竄・期限切れ・削除後)。
func (w *World) expectImageForbidden(ctx context.Context, u *User, imageURL, what string) {
	_, err := w.client.Do(ctx, u.Session(), http.MethodGet, imageURL, "GET /api/images/:id (403)", nil, "", http.StatusForbidden)
	if err != nil {
		var e *client.Error
		if errors.As(err, &e) && e.Got == http.StatusOK {
			w.report(true, u, what, fmt.Errorf("403 であるべき画像が取得できました"))
			return
		}
		w.report(false, u, what, err)
	}
}

func tamperSignature(imageURL string) (string, string) {
	parsed, err := url.Parse(imageURL)
	if err != nil {
		return "", ""
	}
	q := parsed.Query()
	sig := q.Get("sig")
	if sig == "" {
		return "", ""
	}
	b := []byte(sig)
	if b[0] == 'A' {
		b[0] = 'B'
	} else {
		b[0] = 'A'
	}
	q.Set("sig", string(b))
	tampered := *parsed
	tampered.RawQuery = q.Encode()
	q2 := parsed.Query()
	q2.Set("exp", "1700000000")
	expired := *parsed
	expired.RawQuery = q2.Encode()
	return tampered.String(), expired.String()
}

func (w *World) deletePost(ctx context.Context, u *User, p *Post) bool {
	_, err := w.client.Do(ctx, u.Session(), http.MethodDelete, "/api/posts/"+p.ID, "DELETE /api/posts/:id", nil, "", http.StatusNoContent)
	if err != nil {
		w.report(false, u, "投稿を削除できません", err)
		return false
	}
	now := time.Now()
	p.deletedAt.Store(now.UnixNano())
	u.mu.Lock()
	u.deletedImageURLs = append(u.deletedImageURLs, deletedImage{URL: p.ImageURL, DeletedAt: now})
	u.mu.Unlock()
	return true
}

func (w *World) updatePost(ctx context.Context, u *User, p *Post, rng *rand.Rand) {
	caption := captions[rng.IntN(len(captions))] + " (edited)"
	p.captionUpdated.Store(true)
	var res api.Post
	_, err := w.client.JSON(ctx, u.Session(), http.MethodPatch, "/api/posts/"+p.ID, "PATCH /api/posts/:id", map[string]string{"caption": caption}, http.StatusOK, &res)
	if err != nil {
		w.report(false, u, "キャプションを変更できません", err)
		return
	}
	if res.ID != p.ID || res.Caption != caption {
		w.report(true, u, "キャプションの変更", fmt.Errorf("応答のキャプションが一致しません: %q", res.Caption))
		return
	}
	p.Caption = caption
	p.captionUpdated.Store(true)
	w.board.Add(score.PostUpdate, u.Region())
}

// ---- フィード ----

type feedPage struct {
	items   []api.Post
	hasMore bool
}

// feed は 1 ページ取り、内容を検証して加点する。
func (w *World) feed(ctx context.Context, u *User, kind string, before *api.Post, counter *score.WindowCounter) (*feedPage, error) {
	path := "/api/feeds/" + kind
	if before != nil {
		q := url.Values{}
		q.Set("beforePostedAt", before.PostedAt.UTC().Format(time.RFC3339Nano))
		q.Set("beforeId", before.ID)
		if kind == "friends-of-friends" && before.Score != nil {
			q.Set("beforeScore", fmt.Sprint(*before.Score))
		}
		path += "?" + q.Encode()
	}
	start := time.Now()
	var res api.FeedResponse
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, path, "GET /api/feeds/"+kind, nil, http.StatusOK, &res)
	if err != nil {
		w.report(false, u, "フィードを取得できません", err)
		return nil, err
	}
	if len(res.Items) > 50 {
		w.report(true, u, "フィード", fmt.Errorf("1 ページに %d 件 (50 件まで)", len(res.Items)))
	}
	if !w.verifyFeedItems(u, kind, res.Items, start) {
		return nil, errors.New("feed verification failed")
	}
	var seen []*Post
	for _, it := range res.Items {
		if p := w.post(it.ID); p != nil {
			seen = append(seen, p)
		} else {
			// 知らない投稿 (manifest 外の初期データなど) も閲覧候補にする
			seen = append(seen, &Post{ID: it.ID, AuthorID: it.Author.ID, WindowID: it.PostingWindowID, ImageURL: it.ImageURL, Caption: it.Caption, Visibility: it.Visibility, PostedAt: it.PostedAt})
		}
	}
	u.remember(seen...)
	if counter == nil || counter.Take(score.FeedView) {
		w.board.Add(score.FeedView, u.Region())
	}
	return &feedPage{items: res.Items, hasMore: res.HasMore}, nil
}

// verifyFeedItems は「存在しない投稿や閲覧権限のない投稿が返される」ことを検査する (致命)。
func (w *World) verifyFeedItems(u *User, kind string, items []api.Post, start time.Time) bool {
	var prev *api.Post
	for i := range items {
		it := &items[i]
		if it.ID == "" || it.Author.ID == "" || it.ImageURL == "" || it.PostingWindowID == "" {
			w.report(true, u, "フィード", fmt.Errorf("項目のフィールドが欠けています (id=%q)", it.ID))
			return false
		}
		if it.Author.ID == u.ID {
			w.report(true, u, "フィード", fmt.Errorf("自分の投稿 %s がフィードに含まれています", it.ID))
			return false
		}
		if kind == "public" && it.Visibility != "public" {
			w.report(true, u, "フィード", fmt.Errorf("public フィードに visibility=%s の投稿 %s", it.Visibility, it.ID))
			return false
		}
		if kind == "friends-of-friends" {
			if it.Score == nil {
				w.report(true, u, "フィード", fmt.Errorf("friends-of-friends の項目に score がありません"))
				return false
			}
			if prev != nil && prev.Score != nil && *prev.Score < *it.Score {
				w.report(true, u, "フィード", fmt.Errorf("friends-of-friends の score が降順ではありません"))
				return false
			}
		} else if prev != nil && it.PostedAt.After(prev.PostedAt) {
			w.report(true, u, "フィード", fmt.Errorf("フィードが postedAt の降順ではありません"))
			return false
		}
		prev = it
		if p := w.post(it.ID); p != nil {
			if p.DeletedBefore(start) {
				w.report(true, u, "フィード", fmt.Errorf("削除された投稿 %s がフィードに含まれています", it.ID))
				return false
			}
			if p.AuthorID != it.Author.ID {
				w.report(true, u, "フィード", fmt.Errorf("投稿 %s の author が一致しません", it.ID))
				return false
			}
		}
		if win := w.window(it.PostingWindowID); win != nil && win.EndedBefore(start.Add(-time.Second)) {
			w.report(true, u, "フィード", fmt.Errorf("終了した投稿期間 (%s %d 日目) の投稿 %s がフィードに含まれています", win.Region, win.Day, it.ID))
			return false
		}
		author := w.user(it.Author.ID)
		if author == nil {
			continue
		}
		if author.DeletedBefore(start) {
			w.report(true, u, "フィード", fmt.Errorf("退会した利用者 %s の投稿がフィードに含まれています", author.Username))
			return false
		}
		if u.BlockedEitherBefore(author.ID, start) {
			w.report(true, u, "フィード", fmt.Errorf("ブロック関係にある %s の投稿がフィードに含まれています", author.Username))
			return false
		}
		switch kind {
		case "friends":
			if !u.EverFriend(author.ID) {
				w.report(true, u, "フィード", fmt.Errorf("友達でない %s の投稿が friends フィードに含まれています", author.Username))
				return false
			}
		case "following":
			if !u.IsFollowing(author.ID) {
				// フォローの解除直後なら許す。認定公開アカウント以外はフォローできないので致命
				if !author.AccountTypeIs("certified_public") {
					w.report(true, u, "フィード", fmt.Errorf("フォローしていない %s の投稿が following フィードに含まれています", author.Username))
					return false
				}
			}
		case "friends-of-friends":
			if it.Visibility == "friends" {
				w.report(true, u, "フィード", fmt.Errorf("visibility=friends の投稿 %s が friends-of-friends フィードに含まれています", it.ID))
				return false
			}
		}
	}
	return true
}

func (u *User) AccountTypeIs(t string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.AccountType == t
}

// findInFeed は viewer の friends フィードに post があるか探す (最大 pages ページ)。
func (w *World) findInFeed(ctx context.Context, viewer *User, post *Post, pages int, counter *score.WindowCounter) (bool, error) {
	var before *api.Post
	for i := 0; i < pages; i++ {
		page, err := w.feed(ctx, viewer, "friends", before, counter)
		if err != nil {
			return false, err
		}
		for j := range page.items {
			if page.items[j].ID == post.ID {
				it := &page.items[j]
				if it.Visibility != post.Visibility || it.ImageURL == "" || (it.Caption != post.Caption && w.captionMismatch(viewer, post, it.Caption, "friends feed")) {
					w.report(true, viewer, "友達フィードの検証", fmt.Errorf("投稿 %s の内容が一致しません", post.ID))
					return false, errors.New("mismatch")
				}
				return true, nil
			}
		}
		if !page.hasMore || len(page.items) == 0 {
			return false, nil
		}
		before = &page.items[len(page.items)-1]
	}
	return false, nil
}

// ---- 投稿詳細・交流 ----

func (w *World) getPost(ctx context.Context, u *User, p *Post, counter *score.WindowCounter) {
	var res api.Post
	start := time.Now()
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, "/api/posts/"+p.ID, "GET /api/posts/:id", nil, http.StatusOK, &res)
	if err != nil {
		var e *client.Error
		if errors.As(err, &e) && e.Got == http.StatusNotFound {
			if p.Deleted() || w.windowEnded(p) || w.notVisibleReason(u, p, time.Now()) != "" || !w.postedToday(u) {
				return // 見えなくなったのは仕様
			}
		}
		w.report(false, u, "投稿を取得できません", err)
		return
	}
	if res.ID != p.ID || res.Author.ID != p.AuthorID {
		w.report(true, u, "投稿詳細", fmt.Errorf("投稿 %s の内容が一致しません", p.ID))
		return
	}
	if reason := w.notVisibleReason(u, p, start); reason != "" {
		w.report(true, u, "投稿詳細", fmt.Errorf("閲覧権限のない投稿 %s を取得できました (%s)", p.ID, reason))
		return
	}
	if counter.Take(score.PostGet) {
		w.board.Add(score.PostGet, u.Region())
	}
}

func (w *World) windowEnded(p *Post) bool {
	win := w.window(p.WindowID)
	return win != nil && win.Ended()
}

// notVisibleReason は、ベンチの把握している関係から「見えてはいけない」と断定できる理由。空なら判定しない。
func (w *World) notVisibleReason(u *User, p *Post, at time.Time) string {
	author := w.user(p.AuthorID)
	if author == nil || p.AuthorID == u.ID {
		return ""
	}
	if author.DeletedBefore(at) {
		return "退会した利用者の投稿"
	}
	if u.BlockedEitherBefore(author.ID, at) {
		return "ブロック関係"
	}
	if p.Visibility == "friends" && !u.EverFriend(author.ID) {
		return "友達限定の投稿で友達ではない"
	}
	return ""
}

// expectPostHidden は閲覧権限のない投稿が 404 になることを確かめる。
func (w *World) expectPostHidden(ctx context.Context, u *User, p *Post) {
	_, err := w.client.Do(ctx, u.Session(), http.MethodGet, "/api/posts/"+p.ID, "GET /api/posts/:id (404)", nil, "", http.StatusNotFound)
	if err != nil {
		var e *client.Error
		if errors.As(err, &e) && e.Got == http.StatusOK {
			w.report(true, u, "閲覧制限", fmt.Errorf("閲覧権限のない投稿 %s を取得できました", p.ID))
			return
		}
		w.report(false, u, "閲覧制限の確認", err)
	}
}

func (w *World) listRealMojis(ctx context.Context, u *User, p *Post, counter *score.WindowCounter) {
	var res api.RealMojisResponse
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, "/api/posts/"+p.ID+"/realmojis", "GET /api/posts/:id/realmojis", nil, http.StatusOK, &res)
	if err != nil {
		var e *client.Error
		if errors.As(err, &e) && e.Got == http.StatusForbidden && (p.Deleted() || w.windowEnded(p) || w.notVisibleReason(u, p, time.Now()) != "" || !w.postedToday(u)) {
			return
		}
		w.report(false, u, "RealMoji 一覧を取得できません", err)
		return
	}
	for _, r := range res.Items {
		if r.ID == "" || r.Reactor.ID == "" || r.ImageURL == "" || r.ReactionType == "" {
			w.report(true, u, "RealMoji 一覧", fmt.Errorf("項目のフィールドが欠けています"))
			return
		}
	}
	if counter.Take(score.InteractionList) {
		w.board.Add(score.InteractionList, u.Region())
	}
}

func (w *World) listComments(ctx context.Context, u *User, p *Post, counter *score.WindowCounter) {
	var res api.CommentsResponse
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, "/api/posts/"+p.ID+"/comments", "GET /api/posts/:id/comments", nil, http.StatusOK, &res)
	if err != nil {
		var e *client.Error
		if errors.As(err, &e) && e.Got == http.StatusForbidden {
			return // コメント権限は閲覧より狭い (友達か public)。判定はしない
		}
		w.report(false, u, "コメント一覧を取得できません", err)
		return
	}
	for _, c := range res.Items {
		if c.ID == "" || c.Author.ID == "" {
			w.report(true, u, "コメント一覧", fmt.Errorf("項目のフィールドが欠けています"))
			return
		}
	}
	if counter.Take(score.InteractionList) {
		w.board.Add(score.InteractionList, u.Region())
	}
}

var reactionTypes = []string{"like", "smile", "surprise", "heart", "laugh", "instant"}

func (w *World) sendRealMoji(ctx context.Context, u *User, p *Post, rng *rand.Rand, counter *score.WindowCounter) {
	img := w.realmojiImages.Pick(rng)
	reaction := reactionTypes[rng.IntN(len(reactionTypes))]
	body, ct, err := client.Multipart(map[string]string{"reactionType": reaction}, client.FilePart{Field: "image", Filename: "face.jpg", Data: img.Data})
	if err != nil {
		return
	}
	start := time.Now()
	resp, err := w.client.Do(ctx, u.Session(), http.MethodPost, "/api/posts/"+p.ID+"/realmojis", "POST /api/posts/:id/realmojis", body, ct, http.StatusCreated)
	if err != nil {
		var e *client.Error
		if errors.As(err, &e) && (e.Got == http.StatusForbidden || e.Got == http.StatusNotFound) && (p.Deleted() || w.windowEnded(p) || w.notVisibleReason(u, p, time.Now()) != "" || !w.postedToday(u)) {
			return
		}
		if errors.As(err, &e) && e.Got == http.StatusNotFound {
			if author := w.user(p.AuthorID); author == nil || author.Deleted() {
				return // 投稿者が退会した (relationship_not_found)
			}
		}
		w.report(false, u, "RealMoji を送れません", err)
		return
	}
	var res api.RealMoji
	if err := json.Unmarshal(resp.Body, &res); err != nil || res.ID == "" || res.Reactor.ID != u.ID || res.ReactionType != reaction {
		w.report(true, u, "RealMoji の応答", fmt.Errorf("応答が一致しません"))
		return
	}
	if reason := w.notVisibleReason(u, p, start); reason != "" {
		w.report(true, u, "RealMoji", fmt.Errorf("閲覧権限のない投稿 %s への RealMoji が受理されました (%s)", p.ID, reason))
		return
	}
	if counter.Take(score.RealMojiSend) {
		w.board.Add(score.RealMojiSend, u.Region())
	}
	// 自分の RealMoji 画像を確かめ、ときどき削除する
	if w.chance(rng, 0.3) {
		w.getImage(ctx, u, res.ImageURL, &Post{ImageSHA: img.SHA256, HasSHA: true}, "", nil)
	}
	if w.chance(rng, 0.15) {
		if _, err := w.client.Do(ctx, u.Session(), http.MethodDelete, "/api/realmojis/"+res.ID, "DELETE /api/realmojis/:id", nil, "", http.StatusNoContent); err != nil {
			w.report(false, u, "RealMoji を削除できません", err)
		} else {
			w.board.Add(score.InteractionDelete, u.Region())
		}
	}
}

// postedToday は u が自分のリージョンの開いている投稿期間に投稿済みか。
func (w *World) postedToday(u *User) bool {
	rs := w.regions[u.Region()]
	rs.mu.Lock()
	cur := rs.current
	rs.mu.Unlock()
	return cur != nil && u.PostInWindow(cur.ID) != nil
}

var commentBodies = []string{"いいね！", "nice chair", "素敵", "Where is this?", "👍", "Magnifique", "Sehr schön", "lol"}

func (w *World) sendComment(ctx context.Context, u *User, p *Post, rng *rand.Rand, counter *score.WindowCounter) {
	body := commentBodies[rng.IntN(len(commentBodies))]
	var res api.Comment
	start := time.Now()
	_, err := w.client.JSON(ctx, u.Session(), http.MethodPost, "/api/posts/"+p.ID+"/comments", "POST /api/posts/:id/comments", map[string]string{"body": body}, http.StatusCreated, &res)
	if err != nil {
		var e *client.Error
		if errors.As(err, &e) && e.Got == http.StatusForbidden {
			if p.Deleted() || w.windowEnded(p) || w.notVisibleReason(u, p, time.Now()) != "" || !w.postedToday(u) || (p.Visibility != "public" && !u.IsFriend(p.AuthorID)) {
				return
			}
			if author := w.user(p.AuthorID); author == nil || author.Deleted() {
				return // 投稿者が分からない (fof の判定ができない) か退会した
			}
		}
		w.report(false, u, "コメントできません", err)
		return
	}
	if res.ID == "" || res.Author.ID != u.ID || res.Body != body {
		w.report(true, u, "コメントの応答", fmt.Errorf("応答が一致しません"))
		return
	}
	if reason := w.notVisibleReason(u, p, start); reason != "" {
		w.report(true, u, "コメント", fmt.Errorf("権限のない投稿 %s へのコメントが受理されました (%s)", p.ID, reason))
		return
	}
	if p.Visibility != "public" && !u.EverFriend(p.AuthorID) && w.user(p.AuthorID) != nil {
		w.report(true, u, "コメント", fmt.Errorf("友達でない %s の非公開投稿へのコメントが受理されました", p.AuthorID))
		return
	}
	if counter.Take(score.CommentPost) {
		w.board.Add(score.CommentPost, u.Region())
	}
	if w.chance(rng, 0.15) {
		if _, err := w.client.Do(ctx, u.Session(), http.MethodDelete, "/api/comments/"+res.ID, "DELETE /api/comments/:id", nil, "", http.StatusNoContent); err != nil {
			w.report(false, u, "コメントを削除できません", err)
		} else {
			w.board.Add(score.InteractionDelete, u.Region())
		}
	}
}

// ---- 利用者 ----

func (w *World) getUser(ctx context.Context, u *User, target *User, counter *score.WindowCounter) {
	var res api.RelationshipUser
	start := time.Now()
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, "/api/users/"+target.ID, "GET /api/users/:id", nil, http.StatusOK, &res)
	if err != nil {
		var e *client.Error
		if errors.As(err, &e) && e.Got == http.StatusNotFound && (target.Deleted() || u.IsBlockedEither(target.ID)) {
			return
		}
		w.report(false, u, "プロフィールを取得できません", err)
		return
	}
	if u.BlockedEitherBefore(target.ID, start) {
		w.report(true, u, "プロフィール", fmt.Errorf("ブロック関係にある %s のプロフィールを取得できました", target.Username))
		return
	}
	if res.ID != target.ID || res.Username != target.Username {
		w.report(true, u, "プロフィール", fmt.Errorf("プロフィールが一致しません (%q)", res.Username))
		return
	}
	target.mu.Lock()
	bio, accountType, display := target.Bio, target.AccountType, target.DisplayName
	target.mu.Unlock()
	if target.profileChanging() {
		// 変更中は新旧どちらの値も正しい。照合せず加点だけする
		if counter == nil || counter.Take(score.ProfileView) {
			w.board.Add(score.ProfileView, u.Region())
		}
		return
	}
	if res.DisplayName != display {
		// 変更後の読み取りが古い。manual.md の打ち切り条件には無いので致命にせず数える (result.json の staleProfile)
		w.staleProfile.Add(1)
		w.log.Warn("表示名の変更後の読み取りが古い", "viewer", u.Username, "user", target.Username, "got", res.DisplayName, "want", display)
		return
	}
	// 友達になる処理が進行中なら DB では既に友達のことがあるので everFriends で判定する
	if accountType != "certified_public" && !u.EverFriend(target.ID) && res.Bio != "" {
		w.report(true, u, "プロフィール", fmt.Errorf("友達でない %s の自己紹介が見えています", target.Username))
		return
	}
	if (accountType == "certified_public" || u.IsFriend(target.ID)) && res.Bio != bio {
		// 自己紹介の変更と同時に読むと食い違うことがある。加点せずに戻る
		return
	}
	if counter == nil || counter.Take(score.ProfileView) {
		w.board.Add(score.ProfileView, u.Region())
	}
}

func (w *World) searchUsers(ctx context.Context, u *User, target *User) {
	q := target.Username
	var res api.SearchUsersResponse
	start := time.Now()
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, "/api/users?q="+url.QueryEscape(q), "GET /api/users", nil, http.StatusOK, &res)
	if err != nil {
		w.report(false, u, "ユーザーを検索できません", err)
		return
	}
	found := false
	for _, it := range res.Items {
		if it.ID == u.ID {
			w.report(true, u, "ユーザー検索", fmt.Errorf("検索結果に自分が含まれています"))
			return
		}
		if it.ID == target.ID {
			found = true
		}
		if other := w.user(it.ID); other != nil && u.BlockedEitherBefore(other.ID, start) {
			w.report(true, u, "ユーザー検索", fmt.Errorf("ブロック関係にある %s が検索結果に含まれています", other.Username))
			return
		}
	}
	if !found && !target.Deleted() && !u.IsBlockedEither(target.ID) && !res.HasMore {
		w.report(true, u, "ユーザー検索", fmt.Errorf("%q で検索しても %s が見つかりません", q, target.Username))
		return
	}
	w.board.Add(score.UserSearch, u.Region())
}

func (w *World) getMe(ctx context.Context, u *User) *api.Me {
	var res api.Me
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, "/api/me", "GET /api/me", nil, http.StatusOK, &res)
	if err != nil {
		w.report(false, u, "自分の情報を取得できません", err)
		return nil
	}
	u.mu.Lock()
	display, bio, region := u.DisplayName, u.Bio, u.region
	u.mu.Unlock()
	if res.ID != u.ID || res.Username != u.Username || res.Region != region {
		w.report(true, u, "自分の情報", fmt.Errorf("GET /api/me が一致しません (id=%q username=%q region=%q)", res.ID, res.Username, res.Region))
		return nil
	}
	if res.DisplayName != display || res.Bio != bio {
		// 変更直後の読み取りが古い。manual.md の打ち切り条件には無いので致命にせず数える (result.json の staleProfile)
		w.staleProfile.Add(1)
		w.log.Warn("GET /api/me が変更前の値を返した", "user", u.Username, "got", res.DisplayName+" / "+res.Bio, "want", display+" / "+bio)
		return nil
	}
	return &res
}

func (w *World) updateProfile(ctx context.Context, u *User, rng *rand.Rand) {
	display := displayNames[rng.IntN(len(displayNames))] + fmt.Sprintf(" %d", rng.IntN(1000))
	bio := []string{"椅子が好き", "Hello from " + u.Region(), "📷", "coffee & chairs", ""}[rng.IntN(5)]
	u.profileChangedAt.Store(time.Now().UnixNano())
	var res api.Me
	_, err := w.client.JSON(ctx, u.Session(), http.MethodPatch, "/api/me", "PATCH /api/me", map[string]string{"displayName": display, "bio": bio}, http.StatusOK, &res)
	if err != nil {
		w.report(false, u, "プロフィールを変更できません", err)
		return
	}
	if res.DisplayName != display || res.Bio != bio {
		w.report(true, u, "プロフィールの変更", fmt.Errorf("応答が一致しません"))
		return
	}
	u.mu.Lock()
	u.DisplayName, u.Bio = display, bio
	u.mu.Unlock()
	u.profileChangedAt.Store(time.Now().UnixNano())
	if w.getMe(ctx, u) != nil {
		w.board.Add(score.ProfileUpdate, u.Region())
	}
}

func (w *World) changeRegion(ctx context.Context, u *User, rng *rand.Rand) {
	from := u.Region()
	var to string
	for {
		to = api.Regions[rng.IntN(len(api.Regions))]
		if to != from {
			break
		}
	}
	var res api.Me
	_, err := w.client.JSON(ctx, u.Session(), http.MethodPatch, "/api/me", "PATCH /api/me", map[string]string{"region": to}, http.StatusOK, &res)
	if err != nil {
		w.report(false, u, "リージョンを変更できません", err)
		return
	}
	if res.Region != to {
		w.report(true, u, "リージョンの変更", fmt.Errorf("応答の region が %q", res.Region))
		return
	}
	w.moveUser(u, from, to)
	w.board.Add(score.ProfileUpdate, to)
	// 元のリージョンのサーバに送ると 403 region_mismatch になるはず
	old := &client.Session{Token: u.Session().Token, Country: countryFor(from, u.ID), ClientIP: u.ClientIP}
	if _, err := w.client.Do(ctx, old, http.MethodGet, "/api/me", "GET /api/me (region_mismatch)", nil, "", http.StatusForbidden); err != nil {
		var e *client.Error
		if errors.As(err, &e) && e.Got == http.StatusOK {
			w.report(true, u, "リージョンの変更", fmt.Errorf("移動前のリージョンのサーバが応答しました"))
		}
	}
}

func (w *World) putProfileImage(ctx context.Context, u *User, rng *rand.Rand) {
	img := w.profileImages.Pick(rng)
	_, err := w.client.Do(ctx, u.Session(), http.MethodPut, "/api/me/profile-image", "PUT /api/me/profile-image", img.Data, "image/jpeg", http.StatusNoContent)
	if err != nil {
		w.report(false, u, "プロフィール画像を変更できません", err)
		return
	}
	u.mu.Lock()
	u.profileImageSHA, u.hasProfileImage = img.SHA256, true
	u.mu.Unlock()
	me := w.getMe(ctx, u)
	if me == nil {
		return
	}
	if me.ProfileImageURL == nil || *me.ProfileImageURL == "" {
		w.report(true, u, "プロフィール画像", fmt.Errorf("変更後も profileImageUrl が空です"))
		return
	}
	if !w.getImage(ctx, u, *me.ProfileImageURL, &Post{ImageSHA: img.SHA256, HasSHA: true}, "", nil) {
		return
	}
	w.board.Add(score.ProfileImage, u.Region())
	if w.chance(rng, 0.3) {
		if _, err := w.client.Do(ctx, u.Session(), http.MethodDelete, "/api/me/profile-image", "DELETE /api/me/profile-image", nil, "", http.StatusNoContent); err != nil {
			w.report(false, u, "プロフィール画像を削除できません", err)
			return
		}
		u.mu.Lock()
		u.hasProfileImage = false
		u.mu.Unlock()
		if me := w.getMe(ctx, u); me != nil {
			if me.ProfileImageURL != nil && *me.ProfileImageURL != "" {
				w.report(true, u, "プロフィール画像の削除", fmt.Errorf("削除後も profileImageUrl が残っています"))
				return
			}
			w.board.Add(score.ProfileImage, u.Region())
		}
	}
}

func (w *World) deleteAccount(ctx context.Context, u *User) {
	sess := u.Session()
	_, err := w.client.Do(ctx, sess, http.MethodPost, "/api/me/account-deletion", "POST /api/me/account-deletion", nil, "", http.StatusNoContent)
	if err != nil {
		w.report(false, u, "退会できません", err)
		return
	}
	region := u.Region()
	u.mu.Lock()
	u.deleted = true
	u.sess = nil
	u.mu.Unlock()
	u.deletedAt.Store(time.Now().UnixNano())
	u.lastLogin.Store(time.Now().UnixNano())
	// 退会後はログインも旧トークンも使えない
	if _, err := w.client.Do(ctx, sess, http.MethodGet, "/api/me", "GET /api/me (deleted)", nil, "", http.StatusUnauthorized); err != nil {
		var e *client.Error
		if errors.As(err, &e) && e.Got == http.StatusOK {
			w.report(true, u, "退会", fmt.Errorf("退会後も旧トークンで応答しました"))
			return
		}
	}
	anon := &client.Session{Country: countryFor(region, u.ID), ClientIP: u.ClientIP}
	if _, err := w.client.JSON(ctx, anon, http.MethodPost, "/api/auth/login", "POST /api/auth/login (deleted)", map[string]string{"username": u.Username, "secretKey": u.SecretKey}, http.StatusUnauthorized, nil); err != nil {
		var e *client.Error
		if errors.As(err, &e) && e.Got == http.StatusOK {
			w.report(true, u, "退会", fmt.Errorf("退会後もログインできました"))
			return
		}
	}
	w.board.Add(score.AccountDeletion, region)
}

// ---- 関係 ----

func (w *World) friendRequest(ctx context.Context, u, target *User) (string, bool) {
	// 相手が先に申請していれば承認になり、DB では即 友達になる。ベンチの把握が遅れて
	// 「友達でない人の投稿がフィードにある」と誤判定しないよう、送る前に候補として覚える。
	markEverFriends(u, target)
	var res api.FriendRequestResponse
	_, err := w.client.JSON(ctx, u.Session(), http.MethodPost, "/api/friend-requests", "POST /api/friend-requests", map[string]string{"recipientId": target.ID}, http.StatusCreated, &res)
	if err != nil {
		var e *client.Error
		if errors.As(err, &e) && (e.Got == http.StatusConflict || e.Got == http.StatusNotFound) {
			// 既に友達・申請済み・ブロック・退会は仕様どおり
			if u.IsFriend(target.ID) || target.Deleted() || u.IsBlockedEither(target.ID) || e.Got == http.StatusConflict {
				return "", false
			}
		}
		w.report(false, u, "友達申請できません", err)
		return "", false
	}
	if res.RelationID == "" {
		w.report(true, u, "友達申請", fmt.Errorf("relationId が空です"))
		return "", false
	}
	w.board.Add(score.FriendRequest, u.Region())
	if res.Status == "accepted" {
		// 相手から先に申請が来ていた場合は承認になる
		makeFriends(u, target)
		return res.RelationID, true
	}
	return res.RelationID, false
}

func (w *World) transitionRequest(ctx context.Context, actor *User, requestID, action string) bool {
	var res api.FriendRequestResponse
	_, err := w.client.JSON(ctx, actor.Session(), http.MethodPost, "/api/friend-requests/"+requestID+"/"+action, "POST /api/friend-requests/:id/"+action, nil, http.StatusOK, &res)
	if err != nil {
		var e *client.Error
		if errors.As(err, &e) && (e.Got == http.StatusForbidden || e.Got == http.StatusConflict || e.Got == http.StatusNotFound) {
			return false // 既に処理済みなど
		}
		w.report(false, actor, "友達申請を処理できません ("+action+")", err)
		return false
	}
	want := map[string]string{"accept": "accepted", "reject": "rejected", "cancel": "cancelled"}[action]
	if res.Status != want {
		w.report(true, actor, "友達申請の処理", fmt.Errorf("status が %q (期待 %q)", res.Status, want))
		return false
	}
	switch action {
	case "accept":
		w.board.Add(score.FriendAccept, actor.Region())
	default:
		w.board.Add(score.FriendRejectCancel, actor.Region())
	}
	return true
}

func (w *World) listRelationships(ctx context.Context, u *User, kind string) *api.RelationshipsResponse {
	var res api.RelationshipsResponse
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, "/api/relationships/"+kind, "GET /api/relationships/:kind", nil, http.StatusOK, &res)
	if err != nil {
		w.report(false, u, "一覧を取得できません ("+kind+")", err)
		return nil
	}
	if kind == "friends" {
		for _, it := range res.Items {
			if other := w.user(it.User.ID); other != nil && !u.EverFriend(other.ID) {
				w.report(true, u, "友達一覧", fmt.Errorf("友達でない %s が友達一覧に含まれています", other.Username))
				return nil
			}
		}
	}
	w.board.Add(score.RelationshipList, u.Region())
	return &res
}

// befriend は u から target に申請し、target が承認する (両者とも成功したら友達)。
func (w *World) befriend(ctx context.Context, u, target *User) bool {
	if u.ID == target.ID || u.IsFriend(target.ID) || target.Deleted() || target.LoggedOut() {
		return false
	}
	id, accepted := w.friendRequest(ctx, u, target)
	if accepted {
		return true
	}
	if id == "" {
		return false
	}
	if !w.ensureLogin(ctx, target) {
		return false
	}
	if w.transitionRequest(ctx, target, id, "accept") {
		makeFriends(u, target)
		return true
	}
	return false
}

func (w *World) follow(ctx context.Context, u, target *User) {
	_, err := w.client.Do(ctx, u.Session(), http.MethodPut, "/api/follows/"+target.ID, "PUT /api/follows/:id", nil, "", http.StatusNoContent)
	if err != nil {
		var e *client.Error
		if errors.As(err, &e) && e.Got == http.StatusConflict {
			return
		}
		w.report(false, u, "フォローできません", err)
		return
	}
	u.mu.Lock()
	u.following[target.ID] = target
	u.mu.Unlock()
	w.board.Add(score.Follow, u.Region())
}

func (w *World) unfollow(ctx context.Context, u, target *User) {
	_, err := w.client.Do(ctx, u.Session(), http.MethodDelete, "/api/follows/"+target.ID, "DELETE /api/follows/:id", nil, "", http.StatusNoContent)
	if err != nil {
		w.report(false, u, "フォローを解除できません", err)
		return
	}
	u.mu.Lock()
	delete(u.following, target.ID)
	u.mu.Unlock()
	w.board.Add(score.Unfollow, u.Region())
}

func (w *World) removeFriend(ctx context.Context, u, target *User) {
	_, err := w.client.Do(ctx, u.Session(), http.MethodDelete, "/api/friendships/"+target.ID, "DELETE /api/friendships/:id", nil, "", http.StatusNoContent)
	if err != nil {
		w.report(false, u, "友達を解除できません", err)
		return
	}
	unfriend(u, target)
	w.board.Add(score.Unfriend, u.Region())
}

func (w *World) block(ctx context.Context, u, target *User) bool {
	_, err := w.client.Do(ctx, u.Session(), http.MethodPut, "/api/blocks/"+target.ID, "PUT /api/blocks/:id", nil, "", http.StatusNoContent)
	if err != nil {
		w.report(false, u, "ブロックできません", err)
		return false
	}
	unfriend(u, target)
	now := time.Now()
	u.mu.Lock()
	u.blocked[target.ID] = now
	delete(u.following, target.ID)
	u.mu.Unlock()
	target.mu.Lock()
	target.blockedBy[u.ID] = now
	delete(target.following, u.ID)
	target.mu.Unlock()
	w.board.Add(score.Block, u.Region())
	return true
}

func (w *World) unblock(ctx context.Context, u, target *User) {
	_, err := w.client.Do(ctx, u.Session(), http.MethodDelete, "/api/blocks/"+target.ID, "DELETE /api/blocks/:id", nil, "", http.StatusNoContent)
	if err != nil {
		w.report(false, u, "ブロックを解除できません", err)
		return
	}
	u.mu.Lock()
	delete(u.blocked, target.ID)
	u.mu.Unlock()
	target.mu.Lock()
	delete(target.blockedBy, u.ID)
	target.mu.Unlock()
	w.board.Add(score.Unblock, u.Region())
}

// ---- Memories / エクスポート ----

// memoriesDate は日付単位の Memories を取り、expect があれば含まれることを確かめる。
func (w *World) memoriesDate(ctx context.Context, u *User, localDate string, expect *Post, kind score.Kind) bool {
	var res api.MemoriesResponse
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, "/api/memories/"+localDate, "GET /api/memories/:date", nil, http.StatusOK, &res)
	if err != nil {
		w.report(false, u, "Memories を取得できません", err)
		return false
	}
	if !w.verifyMemories(u, res.Items, map[string]bool{localDate: true}) {
		return false
	}
	if expect != nil {
		found := false
		for _, it := range res.Items {
			if it.ID == expect.ID {
				found = true
				if it.Visibility != expect.Visibility || (it.Caption != expect.Caption && w.captionMismatch(u, expect, it.Caption, "memories")) {
					w.report(true, u, "Memories", fmt.Errorf("投稿 %s の内容が一致しません", expect.ID))
					return false
				}
			}
		}
		if !found && !expect.Deleted() {
			w.report(true, u, "Memories", fmt.Errorf("%s の投稿 %s が Memories にありません", localDate, expect.ID))
			return false
		}
	}
	w.board.Add(kind, u.Region())
	return true
}

func (w *World) memoriesMonth(ctx context.Context, u *User, month string) bool {
	var res api.MemoriesResponse
	_, err := w.client.JSON(ctx, u.Session(), http.MethodGet, "/api/memories?month="+month, "GET /api/memories", nil, http.StatusOK, &res)
	if err != nil {
		w.report(false, u, "Memories (月) を取得できません", err)
		return false
	}
	dates := map[string]bool{}
	expected := map[string]*Post{}
	for _, p := range u.Posts() {
		if strings.HasPrefix(p.LocalDate, month) && !p.Deleted() {
			dates[p.LocalDate] = true
			expected[p.ID] = p
		}
	}
	if !w.verifyMemories(u, res.Items, nil) {
		return false
	}
	got := map[string]bool{}
	for _, it := range res.Items {
		got[it.ID] = true
		if !strings.HasPrefix(it.PostedAt.UTC().Format("2006-01"), month[:4]) && false {
			return false
		}
	}
	if !res.HasMore {
		for id, p := range expected {
			if !got[id] {
				w.report(true, u, "Memories (月)", fmt.Errorf("%s の投稿 %s が %s の Memories にありません", p.LocalDate, id, month))
				return false
			}
		}
		if len(res.Items) > len(expected) && len(expected) > 0 && !u.Initial && !u.uncertainPosts.Load() {
			// 自分の投稿を全部知っている利用者なら件数も一致するはず
			w.report(true, u, "Memories (月)", fmt.Errorf("%s の Memories が %d 件 (期待 %d 件)", month, len(res.Items), len(expected)))
			return false
		}
	}
	w.board.Add(score.MemoriesMonth, u.Region())
	return true
}

func (w *World) verifyMemories(u *User, items []api.MemoryPost, dates map[string]bool) bool {
	var prev *api.MemoryPost
	for i := range items {
		it := &items[i]
		if it.Author.ID != u.ID {
			w.report(true, u, "Memories", fmt.Errorf("他人の投稿 %s が Memories に含まれています", it.ID))
			return false
		}
		if p := w.post(it.ID); p != nil {
			if p.Deleted() {
				w.report(true, u, "Memories", fmt.Errorf("削除した投稿 %s が Memories に含まれています", it.ID))
				return false
			}
			if p.Caption != it.Caption && w.captionMismatch(u, p, it.Caption, "memories") {
				w.report(true, u, "Memories", fmt.Errorf("投稿 %s のキャプションが一致しません (got %q, want %q)", it.ID, it.Caption, p.Caption))
				return false
			}
			if dates != nil && !dates[p.LocalDate] {
				w.report(true, u, "Memories", fmt.Errorf("別の日の投稿 %s (%s) が含まれています", it.ID, p.LocalDate))
				return false
			}
		}
		if prev != nil && it.PostedAt.After(prev.PostedAt) {
			w.report(true, u, "Memories", fmt.Errorf("postedAt の降順ではありません"))
			return false
		}
		prev = it
	}
	return true
}

// export は ZIP を取り、posts.json と画像を自分の投稿と照合する。
func (w *World) export(ctx context.Context, u *User) bool {
	resp, err := w.client.Do(ctx, u.Session(), http.MethodGet, "/api/me/export", "GET /api/me/export", nil, "", http.StatusOK)
	if err != nil {
		w.report(false, u, "エクスポートできません", err)
		return false
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/zip") {
		w.report(true, u, "エクスポート", fmt.Errorf("Content-Type が application/zip ではありません: %q", ct))
		return false
	}
	zr, err := zip.NewReader(bytes.NewReader(resp.Body), int64(len(resp.Body)))
	if err != nil {
		w.report(true, u, "エクスポート", fmt.Errorf("ZIP を開けません: %w", err))
		return false
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	pj := files["posts.json"]
	if pj == nil {
		w.report(true, u, "エクスポート", fmt.Errorf("posts.json がありません"))
		return false
	}
	rc, err := pj.Open()
	if err != nil {
		w.report(true, u, "エクスポート", err)
		return false
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		w.report(true, u, "エクスポート", err)
		return false
	}
	var items []api.ExportPost
	if err := json.Unmarshal(data, &items); err != nil {
		w.report(true, u, "エクスポート", fmt.Errorf("posts.json を解釈できません: %w", err))
		return false
	}
	known := map[string]*Post{}
	for _, p := range u.Posts() {
		if !p.Deleted() {
			known[p.ID] = p
		}
	}
	got := map[string]bool{}
	var prev *api.ExportPost
	for i := range items {
		it := &items[i]
		got[it.ID] = true
		if it.Author.ID != u.ID {
			w.report(true, u, "エクスポート", fmt.Errorf("他人の投稿 %s が含まれています", it.ID))
			return false
		}
		if prev != nil && it.PostedAt.Before(prev.PostedAt) {
			w.report(true, u, "エクスポート", fmt.Errorf("posts.json が postedAt の昇順ではありません"))
			return false
		}
		prev = it
		f := files[it.ImagePath]
		if f == nil {
			w.report(true, u, "エクスポート", fmt.Errorf("画像 %s が ZIP にありません", it.ImagePath))
			return false
		}
		p := known[it.ID]
		if p == nil {
			if w.post(it.ID) != nil && w.post(it.ID).Deleted() {
				w.report(true, u, "エクスポート", fmt.Errorf("削除した投稿 %s が含まれています", it.ID))
				return false
			}
			continue
		}
		if p.Visibility != it.Visibility || (p.Caption != it.Caption && w.captionMismatch(u, p, it.Caption, "export")) {
			w.report(true, u, "エクスポート", fmt.Errorf("投稿 %s の内容が一致しません", it.ID))
			return false
		}
		if p.HasSHA {
			rc, err := f.Open()
			if err != nil {
				w.report(true, u, "エクスポート", err)
				return false
			}
			h := sha256.New()
			_, err = io.Copy(h, rc)
			rc.Close()
			if err != nil {
				w.report(true, u, "エクスポート", err)
				return false
			}
			var sum [32]byte
			copy(sum[:], h.Sum(nil))
			if sum != p.ImageSHA {
				w.report(true, u, "エクスポート", fmt.Errorf("画像 %s の内容が一致しません", it.ImagePath))
				return false
			}
		}
	}
	for id, p := range known {
		if !got[id] {
			w.report(true, u, "エクスポート", fmt.Errorf("投稿 %s (%s) がエクスポートにありません", id, p.LocalDate))
			return false
		}
	}
	if !u.Initial && !u.uncertainPosts.Load() && len(items) != len(known) {
		w.report(true, u, "エクスポート", fmt.Errorf("投稿が %d 件 (期待 %d 件)", len(items), len(known)))
		return false
	}
	w.board.Add(score.Export, u.Region())
	return true
}
