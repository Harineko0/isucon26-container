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
		// セッションの確認・閲覧可否 (isureal_post_access)・一覧の取得を 1 文・接続 1 本で行う (#82)。
		// 閲覧できないか一覧が空なら番兵の行 (found = false) が 1 行返る。
		var rows []struct {
			SessRegion *string `db:"sess_region"`
			Allowed    bool    `db:"allowed"`
			Found      bool    `db:"found"`
			RealMojiWithReactor
		}
		if err := selectWithSession(c, h, &rows, func() (int, *string) {
			if len(rows) == 0 {
				return 0, nil
			}
			return len(rows), rows[0].SessRegion
		}, `
   SELECT s.sess_region, a.can_view AS allowed, r.id IS NOT NULL AS found,
          COALESCE(r.id, '') AS id, COALESCE(r.post_id, '') AS post_id, COALESCE(r.reactor_id, '') AS reactor_id,
          COALESCE(r.image_id, '') AS image_id, COALESCE(r.reaction_type, '') AS reaction_type,
          COALESCE(r.created_at, 'epoch') AS created_at, r.replaced_at, r.deleted_at,
          `+userColumnsCoalesced("r", "reactor")+`
   FROM {session}
   CROSS JOIN LATERAL isureal_post_access(s.sess_user_id, $1) a
   LEFT JOIN LATERAL (
     SELECT m.id, m.post_id, m.reactor_id, m.image_id, m.reaction_type, m.created_at, m.replaced_at, m.deleted_at,
            `+userColumnsAs("u", "reactor")+`
     FROM realmojis m JOIN users u ON u.id = m.reactor_id AND u.status = 'active'
     WHERE a.can_view AND m.replaced_at IS NULL AND m.deleted_at IS NULL AND m.post_id=$1 AND ($2::timestamptz IS NULL OR (m.created_at,m.id)<($2,$3))
     ORDER BY m.created_at DESC,m.id DESC LIMIT 51
   ) r ON true
   ORDER BY r.created_at DESC, r.id DESC
  `, input.PostID, input.BeforeCreatedAt, input.BeforeID); err != nil {
			return err
		}
		if len(rows) == 0 || !rows[0].Allowed {
			return NewAPIError(http.StatusForbidden, "interaction_forbidden")
		}
		reactions := make([]RealMojiWithReactor, 0, len(rows))
		for _, row := range rows {
			if !row.Found {
				continue
			}
			reaction := row.RealMojiWithReactor
			if err := reaction.Reactor.setImageKey(); err != nil {
				return err
			}
			reactions = append(reactions, reaction)
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
		var row struct {
			SessRegion *string `db:"sess_region"`
			Result     *string `db:"result"`
		}
		if err := callWithSession(c, h, &row, func() *string { return row.SessRegion }, `(SELECT isureal_delete_realmoji({user}, $1) AS result)`, input.RealMojiID); err != nil {
			return err
		}
		result := ""
		if row.Result != nil {
			result = *row.Result
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
