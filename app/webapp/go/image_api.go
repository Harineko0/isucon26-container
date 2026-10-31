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
		// 所有者の署名鍵を覚えていれば、署名の確認は DB に問い合わせる前に済ませる (#95)。
		// 鍵は不変なので判定は DB で確かめた場合と同じ。無効な署名は asia1 で 1 ベンチ 550 件あり、
		// 満杯のプール (#68) で接続を待ってから 403 にしていた。
		knownKey, hasKey := loadImageKey(input.ImageID)
		if hasKey && !hmac.Equal(sig, imageSignature(knownKey, input.ImageID, exp)) {
			c.Set("abuse", "invalid_signature")
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		// 本体の在り処: メモリ (cached)、ディスク (accelURI。本体は Go では読まず nginx に返させる)、どちらでもない。
		cached, accelURI, hasData := imageDataStore.locate(input.ImageID)
		if !hasData {
			prefetchStoredImage(input.ImageID, h.Config.Region)
		}
		// 署名鍵・閲覧可否・(未キャッシュなら)本体を 1 往復で取る。削除状態は毎回確認する。
		request := imageReadRequest{id: input.ImageID, hasData: hasData, ip: c.RealIP()}
		// 画像の created_at は不変なので、手がかり (imageCreatedAt / imagePartitions) があればそのパーティションの表を
		// 直接引く (image_partition.go)。親表を引くと全パーティションの表ロックを取り、asia2 の LockManager が詰まる。
		rel, exact := imageRelationFor(input.ImageID, time.Now())
		object, err := readImageObject(ctx, h, rel, request)
		hinted := err == nil
		if errors.Is(err, sql.ErrNoRows) && !exact {
			// 手がかり (または今のパーティションという推測) が外れていた場合に備えて親表で引き直す。
			// DB から教わった葉 (exact) に無ければ画像は消えているので、そのまま 403 にする。
			object, err = readImageObject(ctx, h, "image_objects", request)
		}
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
		if !hasKey {
			rememberImageKey(input.ImageID, key)
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
		if accelURI != "" {
			// 本体はディスクのファイル。nginx の internal location が sendfile で返す
			// (infra/app/etc/nginx/sites-available/isureal.conf)。Content-Type と Cache-Control は
			// nginx が X-Accel-Redirect の応答から引き継ぐので、従来の応答と同じヘッダになる。
			c.Response().Header().Set(echo.HeaderContentType, object.MediaType)
			c.Response().Header().Set("X-Accel-Redirect", accelURI)
			return c.NoContent(http.StatusOK)
		}
		return c.Blob(http.StatusOK, object.MediaType, object.Data)
	}
}
