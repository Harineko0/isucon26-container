package isureal

import (
	"bytes"
	"database/sql"
	"errors"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"time"

	"example.com/isureal-company/isureal/internal/jpegcheck"
	"github.com/google/uuid"
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

func createPostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createPostRequest
		if err := c.Bind(&input); err != nil {
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
		imageFile, err := input.Image.Open()
		if err != nil {
			return err
		}
		defer imageFile.Close()
		imageData, err := io.ReadAll(imageFile)
		if err != nil {
			return err
		}
		// Both checks give the same 400, so reject oversized images from the
		// header before validating the whole entropy-coded stream.
		if cfg, err := jpeg.DecodeConfig(bytes.NewReader(imageData)); err != nil || cfg.Width > 4096 || cfg.Height > 4096 {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}
		if _, _, err := jpegcheck.Validate(bytes.NewReader(imageData)); err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_post")
		}

		// 投稿時刻 (posted_at) は isureal_create_post の中で DB の時計から決まり、o_posted_at で返る
		// (投稿期間の starts_at / ended_at と同じ時計にそろえる。アプリの時計で先に決めると、
		// 期間の切り替えを待った投稿が starts_at より前の時刻になり 409 になっていた)。
		imageID := uuid.NewString()
		postID := uuid.NewString()
		var row struct {
			SessRegion     *string    `db:"sess_region"`
			Result         string     `db:"o_result"`
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
		if err := callWithSession(c, h, &row, func() *string { return row.SessRegion }, `isureal_create_post({user}, $1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			imageID, postID, imageData, input.Caption, input.Visibility,
			location.Kind, location.Latitude, location.Longitude, location.Locality); err != nil {
			return err
		}
		switch row.Result {
		case "ok":
		case "slot_unavailable":
			return NewAPIError(http.StatusConflict, "post_slot_unavailable")
		default:
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
		ctx := c.Request().Context()
		userID := GetSession(c).UserID
		var posts []PostWithAuthor
		err := h.DB.SelectContext(ctx, &posts, `
   SELECT `+postColumns+`, `+userColumnsAs("u", "author")+`
   FROM posts p
   JOIN posting_windows w ON w.id = p.posting_window_id
   JOIN users u ON u.id = p.author_id AND u.status = 'active'
   JOIN posting_windows current ON current.region = u.region AND current.ended_at IS NULL
                              AND current.local_date = w.local_date
   WHERE p.author_id = $1 AND p.deleted_at IS NULL
  `, userID)
		if err != nil {
			return err
		}
		if len(posts) == 0 {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		post, user := posts[0].Post, posts[0].Author
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
		ctx := c.Request().Context()
		post, err, viewer, viewerErr := loadPostAndViewer(ctx, h.DB, input.PostID, GetSession(c).UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		if err != nil {
			return err
		}
		err = viewerErr
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err != nil || !viewer.canView(post.Post) {
			return NewAPIError(http.StatusNotFound, "post_not_found")
		}
		return c.JSON(http.StatusOK, getPostResponse(newPostResponse(post.Post, post.Author)))
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
		ctx := c.Request().Context()
		var result string
		if err := h.DB.GetContext(ctx, &result, `SELECT isureal_update_post($1, $2, $3, $4)`, GetSession(c).UserID, input.PostID, input.Caption, input.Visibility); err != nil {
			return err
		}
		switch result {
		case "ok":
		case "not_found":
			return NewAPIError(http.StatusNotFound, "post_not_found")
		default:
			return NewAPIError(http.StatusForbidden, "post_not_editable")
		}
		updated, err := getPostWithAuthor(ctx, h.DB, input.PostID)
		if err != nil {
			return err
		}
		post, user := updated.Post, updated.Author
		return c.JSON(http.StatusOK, updatePostResponse(newPostResponse(post, user)))
	}
}

type deletePostRequest struct {
	PostID string `param:"postId" json:"-"`
}

func deletePostHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deletePostRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		var result string
		if err := h.DB.GetContext(c.Request().Context(), &result, `SELECT isureal_delete_post($1, $2)`, GetSession(c).UserID, input.PostID); err != nil {
			return err
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
