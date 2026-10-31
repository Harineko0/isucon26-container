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
			Partition     string    `db:"partition"`
		}
		conn, err := h.IPBans.connUnlessBanned(ctx, h.DB, c.RealIP())
		if err != nil {
			return err
		}
		// 画像の created_at は不変なので、手がかり (imageCreatedAt / imagePartitions) があればそのパーティションの表を
		// 直接引く (image_partition.go)。親表を引くと全パーティションの表ロックを取り、asia2 の LockManager が詰まる。
		rel := imageRelationFor(input.ImageID)
		hinted := false
		if rel != "" {
			err = conn.GetContext(ctx, &object, imageQueryFor(rel), input.ImageID, hasData)
			hinted = err == nil
			if errors.Is(err, sql.ErrNoRows) {
				// 手がかりが外れていた場合に備えて親表で引き直す。
				err = conn.GetContext(ctx, &object, imageQueryFor("image_objects"), input.ImageID, hasData)
			}
		} else {
			err = conn.GetContext(ctx, &object, imageQueryFor("image_objects"), input.ImageID, hasData)
		}
		conn.Close()
		if err == nil {
			if !hinted {
				imageCreatedAt.Store(input.ImageID, object.CreatedAt)
			}
			if object.Partition != rel {
				rememberImagePartition(input.ImageID, object.Partition)
			}
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
