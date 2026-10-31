package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"
)

type getImageRequest struct {
	ImageID string `param:"imageId" json:"-"`
	Exp     string `query:"exp" json:"-"`
	Sig     string `query:"sig" json:"-"`
}

func getImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		c.Response().Header().Set("Cache-Control", "no-store")
		var input getImageRequest
		if err := c.Bind(&input); err != nil {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		query, err := url.ParseQuery(c.Request().URL.RawQuery)
		if err != nil || len(query["exp"]) != 1 || len(query["sig"]) != 1 {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		exp, err := strconv.ParseInt(input.Exp, 10, 64)
		if err != nil || strconv.FormatInt(exp, 10) != input.Exp || exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		sig, err := base64.RawURLEncoding.Strict().DecodeString(input.Sig)
		if err != nil || len(sig) != sha256.Size || base64.RawURLEncoding.EncodeToString(sig) != input.Sig {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}

		ctx := c.Request().Context()
		cached, hasData := imageDataStore.get(input.ImageID)
		if !hasData {
			prefetchStoredImage(input.ImageID, h.Config.Region)
		}
		// 署名鍵・閲覧可否・(未キャッシュなら)本体を 1 往復で取る。削除状態は毎回確認する。
		var object struct {
			SecretKeyHash string    `db:"secret_key_hash"`
			MediaType     string    `db:"media_type"`
			Data          []byte    `db:"data"`
			StoredRegion  *string   `db:"stored_region"`
			Allowed       bool      `db:"allowed"`
			CreatedAt     time.Time `db:"created_at"`
		}
		// 画像の created_at は不変なので、手がかり (imageCreatedAt) があれば created_at の範囲も付けてパーティションを絞る。
		imageQuery := `
   SELECT u.secret_key_hash, i.media_type, i.created_at, CASE WHEN $2 THEN NULL ELSE i.data END AS data, i.stored_region, COALESCE(
     u.profile_image_id = i.id
     OR EXISTS (
       SELECT 1 FROM posts p
       WHERE p.image_id = i.id AND p.author_id = u.id AND p.deleted_at IS NULL
     )
     OR EXISTS (
       SELECT 1 FROM realmojis r
       JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
       JOIN users author ON author.id = p.author_id AND author.status = 'active'
       WHERE r.image_id = i.id AND r.reactor_id = u.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
     )
   , false) AS allowed
   FROM image_objects i JOIN users u ON u.id = i.owner_user_id AND u.status = 'active'
   WHERE i.id = $1`
		conn, err := h.IPBans.connUnlessBanned(ctx, h.DB, c.RealIP())
		if err != nil {
			return err
		}
		hinted := false
		if createdAt, ok := imageCreatedAt.Load(input.ImageID); ok {
			// 範囲の両端は STABLE な式 (timestamptz ± interval) にして、プラン時ではなく実行開始時に刈り込ませる。
			// 定数 (= $3) のままだとプラン時に 1 パーティションへ刈り込めるカスタムプランの方が安く見えて
			// 汎用プランに切り替わらず、毎回プランニング (実測 ~1ms) が走る。この形なら 5 回目以降は汎用プラン
			// (プランニング ~0.05ms、実行開始時に 35 パーティションを除外) になる。
			err = conn.GetContext(ctx, &object, imageQuery+` AND i.created_at BETWEEN $3::timestamptz - interval '1 hour' AND $3::timestamptz + interval '0 seconds'`, input.ImageID, hasData, createdAt.(time.Time))
			hinted = err == nil
			if errors.Is(err, sql.ErrNoRows) {
				// 手がかりが外れていた場合に備えて id だけで引き直す。
				err = conn.GetContext(ctx, &object, imageQuery, input.ImageID, hasData)
			}
		} else {
			err = conn.GetContext(ctx, &object, imageQuery, input.ImageID, hasData)
		}
		conn.Close()
		if err == nil && !hinted {
			imageCreatedAt.Store(input.ImageID, object.CreatedAt)
		}
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if err != nil {
			return err
		}
		key, err := imageSigningKey(object.SecretKeyHash)
		if err != nil {
			return err
		}
		if !hmac.Equal(sig, imageSignature(key, input.ImageID, exp)) {
			c.Set("abuse", "invalid_signature")
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if !object.Allowed {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if hasData {
			object.Data = cached
		} else if object.StoredRegion != nil && len(object.Data) == 0 {
			// 本体は stored_region のサーバーのディスクにある (image_store.go)。
			data, err := loadStoredImage(input.ImageID, *object.StoredRegion, h.Config.Region)
			if err != nil {
				// 先読みが失敗していた (その結果を受け取った) 場合に備えて、1 回だけ取り直す。
				data, err = loadStoredImage(input.ImageID, *object.StoredRegion, h.Config.Region)
			}
			if err != nil {
				return err
			}
			object.Data = data
		} else {
			imageDataStore.put(input.ImageID, object.Data)
			saveInitialImageToDisk(input.ImageID, object.CreatedAt, object.Data)
		}
		if exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		return c.Blob(http.StatusOK, object.MediaType, object.Data)
	}
}
