package isureal

import (
	"bytes"
	"fmt"
	"image/jpeg"
	"mime/multipart"
	"net/http"
	"time"

	"example.com/isureal-company/isureal/internal/jpegcheck"
	"github.com/labstack/echo/v5"
)

type replaceRealMojiRequest struct {
	PostID       string                `param:"postId" json:"-"`
	Image        *multipart.FileHeader `form:"image"`
	ReactionType string                `form:"reactionType"`
}

type replaceRealMojiResponse struct {
	ID           string                   `json:"id"`
	Reactor      relationshipUserResponse `json:"reactor"`
	ReactionType string                   `json:"reactionType"`
	ImageURL     string                   `json:"imageUrl"`
	CreatedAt    time.Time                `json:"createdAt"`
}

func newRealMojiResponse(reaction RealMoji, user User) replaceRealMojiResponse {
	// RealMoji の画像は同じトランザクションで realmojis より先に INSERT されるので、
	// realmojis.created_at が画像の created_at の上限になる (画像取得でパーティションを絞る手がかり)。
	imageCreatedAt.Store(reaction.ImageID, reaction.CreatedAt)
	return replaceRealMojiResponse{ID: reaction.ID, Reactor: newRelationshipUserResponse(user), ReactionType: reaction.ReactionType, ImageURL: signedImageURL(user, reaction.ImageID), CreatedAt: reaction.CreatedAt}
}

func replaceRealMojiHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input replaceRealMojiRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if input.Image == nil {
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}
		switch input.ReactionType {
		case "like", "smile", "surprise", "heart", "laugh", "instant":
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}
		file, err := input.Image.Open()
		if err != nil {
			return err
		}
		defer file.Close()
		// multipart が既にメモリに持っている部分なので長さは確定している (#93)。
		data, err := readUpload(file, input.Image.Size)
		if err != nil {
			return err
		}
		// Both checks give the same 400, so reject oversized images from the
		// header before validating the whole entropy-coded stream.
		if cfg, err := jpeg.DecodeConfig(bytes.NewReader(data)); err != nil || cfg.Width > 512 || cfg.Height > 512 {
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}
		if _, _, err := jpegcheck.Validate(bytes.NewReader(data)); err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_realmoji")
		}

		id, imageID := newTimeID(), newImageID(h.Config.Region)
		// 本体はこのサーバーのディスクに fsync してから、DB にはメタデータだけを入れる (image_store.go)。
		dbData, storedRegion := storeUploadedImage(imageID, data, h.Config.Region)
		var row interactionRow
		if err := callWithSession(c, h, &row, func() *string { return row.SessRegion }, `isureal_replace_realmoji({user}, $1, $2, $3, $4, $5, $6)`,
			input.PostID, id, imageID, dbData, input.ReactionType, storedRegion); err != nil {
			return err
		}
		if err := row.err(); err != nil {
			discardUploadedImage(imageID, storedRegion)
			return err
		}
		user, err := row.user()
		if err != nil {
			return err
		}
		imageDataStore.put(imageID, data)
		enqueueImagePush(imageID, data, storedRegion, h.Config.Region)
		reaction := RealMoji{ID: id, PostID: input.PostID, ReactorID: user.ID, ImageID: imageID, ReactionType: input.ReactionType, CreatedAt: *row.CreatedAt}
		return c.JSON(http.StatusCreated, newRealMojiResponse(reaction, user))
	}
}

type getRealMojisRequest struct {
	PostID          string     `param:"postId"`
	BeforeCreatedAt *time.Time `query:"beforeCreatedAt"`
	BeforeID        string     `query:"beforeId"`
}

type getRealMojisResponse struct {
	Items   []replaceRealMojiResponse `json:"items"`
	HasMore bool                      `json:"hasMore"`
}

func getRealMojisHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getRealMojisRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.BeforeCreatedAt == nil) != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		ctx := c.Request().Context()
		var reactions []RealMojiWithReactor
		var listErr error
		done := make(chan struct{})
		go func() {
			defer close(done)
			reactions, listErr = selectCurrentRealMojis(ctx, h.DB, `AND r.post_id=$1 AND ($2::timestamptz IS NULL OR (r.created_at,r.id)<($2,$3)) ORDER BY r.created_at DESC,r.id DESC LIMIT 51`, input.PostID, input.BeforeCreatedAt, input.BeforeID)
		}()
		allowed, _, err := postAccess(ctx, h.DB, GetSession(c).UserID, input.PostID)
		<-done
		if err != nil {
			return err
		}
		if !allowed {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		if listErr != nil {
			return listErr
		}
		plan := newSignaturePlan()
		for _, reaction := range reactions {
			plan.add(reaction.Reactor, reaction.ImageID)
			plan.addProfile(reaction.Reactor)
		}
		plan.run()
		items := make([]replaceRealMojiResponse, 0, len(reactions))
		for _, reaction := range reactions {
			items = append(items, newRealMojiResponse(reaction.RealMoji, reaction.Reactor))
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
		}
		return c.JSON(http.StatusOK, getRealMojisResponse{Items: items, HasMore: hasMore})
	}
}

type deleteRealMojiRequest struct {
	RealMojiID string `param:"realMojiId" json:"-"`
}

func deleteRealMojiHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input deleteRealMojiRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		ctx := c.Request().Context()
		actor := GetSession(c).UserID
		var result string
		if err := h.DB.GetContext(ctx, &result, `SELECT isureal_delete_realmoji($1, $2)`, actor, input.RealMojiID); err != nil {
			return err
		}
		switch result {
		case "ok":
		case "not_found":
			return NewAPIError(http.StatusNotFound, "interaction_not_found")
		case "forbidden":
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		default:
			return fmt.Errorf("unexpected delete realmoji result: %s", result)
		}
		return c.NoContent(http.StatusNoContent)
	}
}
