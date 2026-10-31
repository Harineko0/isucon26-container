package isureal

import (
	"bytes"
	"context"
	"database/sql"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"time"

	"example.com/isureal-company/isureal/internal/jpegcheck"
	"github.com/labstack/echo/v5"
)

type createPostRequest struct {
	Image        *multipart.FileHeader `form:"image"`
	Caption      string                `form:"caption"`
	Visibility   string                `form:"visibility"`
	LocationKind string                `form:"location_kind"`
	Latitude     *float64              `form:"latitude"`
	Longitude    *float64              `form:"longitude"`
	Locality     *string               `form:"locality"`
}

type postAuthorResponse struct {
	ID              string  `json:"id"`
	Username        string  `json:"username"`
	DisplayName     string  `json:"displayName"`
	ProfileImageURL *string `json:"profileImageUrl"`
	Bio             string  `json:"bio"`
	AccountType     string  `json:"accountType"`
}

type createPostResponse struct {
	ID              string             `json:"id"`
	Author          postAuthorResponse `json:"author"`
	PostingWindowID string             `json:"postingWindowId"`
	ImageURL        string             `json:"imageUrl"`
	Caption         string             `json:"caption"`
	Visibility      string             `json:"visibility"`
	Location        PostLocation       `json:"location"`
	PostedAt        time.Time          `json:"postedAt"`
	OnTime          bool               `json:"onTime"`
	LateSeconds     int64              `json:"lateSeconds"`
}

// createPostGate は isureal_create_post のサーバごとの同時実行数を
// 絞る。image_objects の TOAST の拡張ロック (Lock:extend) は、同時に走る backend が 12 本以下なら
// 待ちがほぼ 0 で、asia のプール 80 本では最大 50 本ほどがロック待ちのままプールの接続を占めていた
// (DB の wait の 28%、INSERT の平均 390 ms)。待ちは DB のロックではなく、接続を取り出す前の goroutine で
// させる。RealMoji・プロフィール画像の INSERT (68 ms) は対象にしない。
type createPostGate chan struct{}

// createPostGateSize はサーバ (リージョン) ごとの同時実行数。asia は EU/US の約 5 倍の投稿を受ける。
// 3 台合計で 8 + 4 + 4 = 16 本 (待ちが 0 だった 12 本に近い値。asia に寄せる)。
// asia 8 のベンチ (main 91832e2, 64,912) では画像 INSERT が 390→88 ms に下がった一方、asia の POST /api/posts が
// 平均 1.9 s (最大 4.7 s) と gate 待ちが支配的になり、グランプリの投稿は 400 人中 125 人だった。
// asia 12 のベンチ (main b48193a, 63,766) では asia の POST /api/posts が平均 4.1 s (最大 10.7 s) になり、
// グランプリの投稿は 374 人中 19 人に落ちた (8 のときは 400 人中 125 人)。8 に戻した。
// #128 (画像の本体をリージョンのディスクに置く) 以降、投稿画像は image_objects に data を入れない
// (storeUploadedImage が空の data と stored_region を返す) ので、この枠が避けていた TOAST の拡張ロックは
// 起きない。一方で asia の最終日の POST /api/posts は枠待ちで平均 4.4 s かかり、グランプリの人数を決めていた
// (#97)。枠はプール (asia 80, EU/US 40) より十分小さい値まで広げる。ディスク保存に失敗して DB に data を
// 入れる場合だけは TOAST に書くが、その経路はまれ。
var createPostGateSize = map[string]int{"asia": 24, "europe": 12, "america": 12}

func newCreatePostGate(region string) createPostGate {
	n, ok := createPostGateSize[region]
	if !ok {
		n = 6
	}
	return make(createPostGate, n)
}

// acquire は枠が空くか ctx が取り消されるまで待つ。取り消された要求は DB に行かない。
func (g createPostGate) acquire(ctx context.Context) error {
	select {
	case g <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (g createPostGate) release() { <-g }

func createPostHandler(h *HandlerContext) echo.HandlerFunc {
	gate := newCreatePostGate(h.Config.Region)
	return func(c *echo.Context) error {
		var input createPostRequest
		formImage, err := bindImageForm(c, &input)
		if err != nil {
			return err
		}
		if input.Image == nil || !validUserText(input.Caption, 0, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		switch input.Visibility {
		case "friends", "friends_of_friends", "public":
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		location := PostLocation{
			Kind: input.LocationKind, Latitude: input.Latitude,
			Longitude: input.Longitude, Locality: input.Locality,
		}
		if !location.valid() {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		imageData, err := readFormImage(input.Image, formImage)
		if err != nil {
			return err
		}
		// Both checks give the same 400, so reject oversized images from the
		// header before validating the whole entropy-coded stream.
		if cfg, err := jpeg.DecodeConfig(bytes.NewReader(imageData)); err != nil || cfg.Width > 4096 || cfg.Height > 4096 {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if _, _, err := jpegcheck.ValidateBytes(imageData); err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}

		// 投稿時刻 (posted_at) は isureal_create_post の中で DB の時計から決まり、o_posted_at で返る
		// (投稿期間の starts_at / ended_at と同じ時計にそろえる。アプリの時計で先に決めると、
		// 期間の切り替えを待った投稿が starts_at より前の時刻になり 409 になっていた)。
		imageID := newImageID(h.Config.Region)
		postID := newTimeID()
		// deferred 経路でセッションのリージョンが違うと関数は呼ばれず o_* が NULL になるので、全部ポインタで受ける
		// (string のままだと走査に失敗して callWithSession の 403 に届かず 500 になる、#53)。
		var row struct {
			SessRegion     *string    `db:"sess_region"`
			Result         *string    `db:"o_result"`
			WindowID       *string    `db:"o_window_id"`
			OnTimeDeadline *time.Time `db:"o_on_time_deadline"`
			PostedAt       *time.Time `db:"o_posted_at"`
			Latitude       *float64   `db:"o_latitude"`
			Longitude      *float64   `db:"o_longitude"`
			Locality       *string    `db:"o_locality"`
			ID             *string    `db:"o_id"`
			Username       *string    `db:"o_username"`
			SecretKeyHash  *string    `db:"o_secret_key_hash"`
			DisplayName    *string    `db:"o_display_name"`
			Bio            *string    `db:"o_bio"`
			ProfileImageID *string    `db:"o_profile_image_id"`
			Region         *string    `db:"o_region"`
			AccountType    *string    `db:"o_account_type"`
			Status         *string    `db:"o_status"`
		}
		// 本体はこのサーバーのディスクに fsync してから、DB にはメタデータだけを入れる (image_store.go)。
		// ディスクへの書き込みは gate の外で済ませ、gate では DB の呼び出しだけを絞る。
		dbImageData, storedRegion := storeUploadedImage(imageID, imageData, h.Config.Region)
		if err := gate.acquire(c.Request().Context()); err != nil {
			discardUploadedImage(imageID, storedRegion)
			return err
		}
		err = callWithSession(c, h, &row, func() *string { return row.SessRegion }, `isureal_create_post({user}, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			imageID, postID, dbImageData, input.Caption, input.Visibility,
			location.Kind, location.Latitude, location.Longitude, location.Locality, storedRegion)
		gate.release()
		if err != nil {
			return err
		}
		if row.Result == nil {
			return sql.ErrNoRows
		}
		switch *row.Result {
		case "ok":
		case "slot_unavailable":
			discardUploadedImage(imageID, storedRegion)
			return NewAPIError(http.StatusConflict, "post_slot_unavailable")
		default:
			discardUploadedImage(imageID, storedRegion)
			return sql.ErrNoRows
		}
		user := User{
			ID: *row.ID, Username: *row.Username, SecretKeyHash: *row.SecretKeyHash, DisplayName: *row.DisplayName,
			Bio: *row.Bio, ProfileImageID: row.ProfileImageID, Region: *row.Region, AccountType: *row.AccountType, Status: *row.Status,
		}
		if err := user.setImageKey(); err != nil {
			return err
		}
		post := Post{
			ID: postID, AuthorID: user.ID, PostingWindowID: *row.WindowID, ImageID: imageID,
			Caption: input.Caption, Visibility: input.Visibility,
			PostLocation: PostLocation{Kind: location.Kind, Latitude: row.Latitude, Longitude: row.Longitude, Locality: row.Locality},
			PostedAt: *row.PostedAt, OnTimeDeadline: *row.OnTimeDeadline,
		}
		imageDataStore.put(imageID, imageData)
		enqueueImagePush(imageID, imageData, storedRegion, h.Config.Region)
		return c.JSON(http.StatusCreated, newPostResponse(post, user))
	}
}

func newPostResponse(post Post, user User) createPostResponse {
	// 投稿画像の created_at は投稿時刻と同じ(画像取得でパーティションを絞る手がかり)。
	imageCreatedAt.Store(post.ImageID, post.PostedAt)
	response := createPostResponse{
		ID: post.ID,
		Author: postAuthorResponse{
			ID: user.ID, Username: user.Username, DisplayName: user.DisplayName,
			ProfileImageURL: newGetMeResponse(user).ProfileImageURL,
			Bio:             user.Bio, AccountType: user.AccountType,
		},
		PostingWindowID: post.PostingWindowID, ImageURL: signedImageURL(user, post.ImageID),
		Caption: post.Caption, Visibility: post.Visibility, Location: post.PostLocation,
		PostedAt: post.PostedAt, OnTime: !post.PostedAt.After(post.OnTimeDeadline),
	}
	if !response.OnTime {
		response.LateSeconds = int64(post.PostedAt.Sub(post.OnTimeDeadline) / time.Second)
	}
	return response
}

type getMyCurrentPostResponse createPostResponse

func getMyCurrentPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var row struct {
			SessRegion *string `db:"sess_region"`
			Found      bool    `db:"found"`
			PostWithAuthor
		}
		if err := getWithSession(c, h, &row, func() *string { return row.SessRegion }, `
   SELECT s.sess_region, r.id IS NOT NULL AS found,
          `+postWithAuthorColumnsCoalesced("r")+`
   FROM {session}
   LEFT JOIN LATERAL (
     SELECT `+postColumns+`, `+userColumnsAs("u", "author")+`
     FROM posts p
     JOIN posting_windows w ON w.id = p.posting_window_id
     JOIN users u ON u.id = p.author_id AND u.status = 'active'
     JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                                AND current.local_date = w.local_date
     WHERE p.author_id = s.sess_user_id AND p.deleted_at IS NULL
     LIMIT 1
   ) r ON true
  `); err != nil {
			return err
		}
		if !row.Found {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		post, user := row.Post, row.Author
		if err := user.setImageKey(); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, getMyCurrentPostResponse(newPostResponse(post, user)))
	}
}

type getPostRequest struct {
	PostID string `param:"postId"`
}

type getPostResponse createPostResponse

func getPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getPostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		var row struct {
			SessRegion *string `db:"sess_region"`
			Found      bool    `db:"found"`
			PostWithAuthor
		}
		if err := getWithSession(c, h, &row, func() *string { return row.SessRegion }, `
   SELECT s.sess_region, r.id IS NOT NULL AS found,
          `+postWithAuthorColumnsCoalesced("r")+`
   FROM {session}
   LEFT JOIN LATERAL (
     SELECT `+postColumns+`, `+userColumnsAs("u", "author")+`
     FROM isureal_post_access(s.sess_user_id, $1) a
     JOIN posts p ON a.can_view AND p.id = $1 AND p.deleted_at IS NULL
     JOIN posting_windows w ON w.id = p.posting_window_id
     JOIN users u ON u.id = p.author_id AND u.status = 'active'
   ) r ON true
  `, input.PostID); err != nil {
			return err
		}
		if !row.Found {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err := row.Author.setImageKey(); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, getPostResponse(newPostResponse(row.Post, row.Author)))
	}
}

type updatePostRequest struct {
	PostID     string  `param:"postId" json:"-"`
	Caption    *string `json:"caption"`
	Visibility *string `json:"visibility"`
}

type updatePostResponse createPostResponse

func updatePostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input updatePostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if input.Caption == nil && input.Visibility == nil {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if input.Caption != nil && !validUserText(*input.Caption, 0, 1000) {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if input.Visibility != nil {
			switch *input.Visibility {
			case "friends", "friends_of_friends", "public":
			default:
				return NewAPIError(http.StatusBadRequest, "invalid_post")
			}
		}
		// 更新と更新後の投稿・投稿者の取得を、セッションとリージョンの確認と同じ 1 文で行う (#17)。
		// 以前は callWithSession(isureal_update_post) → getPostWithAuthor の 2 往復だった。
		var row struct {
			SessRegion *string `db:"sess_region"`
			Result     string  `db:"result"`
			PostWithAuthor
		}
		if err := callWithSessionColumns(c, h, &row, func() *string { return row.SessRegion }, updatePostReturningColumns,
			`isureal_update_post_returning({user}, $1, $2, $3)`, input.PostID, input.Caption, input.Visibility); err != nil {
			return err
		}
		switch row.Result {
		case "ok":
		case "not_found":
			return NewAPIError(http.StatusNotFound, "post_not_found")
		default:
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		if row.ID == "" {
			// 更新できたのに投稿者が active でない (以前の getPostWithAuthor の sql.ErrNoRows と同じく 500)。
			return sql.ErrNoRows
		}
		if err := row.Author.setImageKey(); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, updatePostResponse(newPostResponse(row.Post, row.Author)))
	}
}

// updatePostReturningColumns は isureal_update_post_returning の o_* を PostWithAuthor の列名で受ける選択リスト。
// リージョン違いや 'ok' 以外では列が NULL になるので、NOT NULL の列は COALESCE で埋める (値は使わない)。
const updatePostReturningColumns = `COALESCE(r.o_result, '') AS result,
  COALESCE(r.o_id, '') AS id, COALESCE(r.o_author_id, '') AS author_id,
  COALESCE(r.o_posting_window_id, '') AS posting_window_id, COALESCE(r.o_image_id, '') AS image_id,
  COALESCE(r.o_caption, '') AS caption, COALESCE(r.o_visibility, '') AS visibility,
  COALESCE(r.o_location_kind, '') AS location_kind, r.o_latitude AS latitude, r.o_longitude AS longitude,
  r.o_locality AS locality, COALESCE(r.o_posted_at, 'epoch') AS posted_at,
  COALESCE(r.o_on_time_deadline, 'epoch') AS on_time_deadline, r.o_window_ended_at AS window_ended_at,
  COALESCE(r.o_user_id, '') AS "author.id", COALESCE(r.o_username, '') AS "author.username",
  COALESCE(r.o_secret_key_hash, '') AS "author.secret_key_hash", COALESCE(r.o_display_name, '') AS "author.display_name",
  COALESCE(r.o_bio, '') AS "author.bio", r.o_profile_image_id AS "author.profile_image_id",
  COALESCE(r.o_region, '') AS "author.region", COALESCE(r.o_account_type, '') AS "author.account_type",
  COALESCE(r.o_status, '') AS "author.status"`

type deletePostRequest struct {
	PostID string `param:"postId" json:"-"`
}

func deletePostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deletePostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		var row struct {
			SessRegion *string `db:"sess_region"`
			Result     *string `db:"result"`
		}
		if err := callWithSession(c, h, &row, func() *string { return row.SessRegion }, `(SELECT isureal_delete_post({user}, $1) AS result)`, input.PostID); err != nil {
			return err
		}
		result := ""
		if row.Result != nil {
			result = *row.Result
		}
		switch result {
		case "ok":
		case "not_found":
			return NewAPIError(http.StatusNotFound, "post_not_found")
		case "slot_unavailable":
			return NewAPIError(http.StatusConflict, "post_slot_unavailable")
		default:
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		return c.NoContent(http.StatusNoContent)
	}
}
