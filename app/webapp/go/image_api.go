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

// imageAccelRegions は、ディスク (imgcache) にある画像を Go のヒープに載せず X-Accel-Redirect で nginx に返させる (#94)
// リージョン。PR #130 の初回ベンチ (job 21475) では、EU/US の画像取得は 0.235/0.206 → 0.175/0.178 秒と速くなった
// 一方、asia1 は 0.161 → 0.265 秒に遅くなり iowait が 22% に上がった (16GB の imgcache に対してページキャッシュが
// 小さく、ヒープのキャッシュが外れた分がディスク読みになったとみられる)。asia はヒープのキャッシュのまま。
var imageAccelRegions = map[string]bool{"europe": true, "america": true}

func getImageHandler(h *HandlerContext) echo.HandlerFunc {
	useAccel := imageAccelRegions[h.Config.Region]
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
		// 本体はヒープ → ディスク (imgcache) の順に探す。useAccel のリージョンでは、ディスクにある画像はヒープに
		// 載せず、閲覧可否を確かめたあと X-Accel-Redirect で nginx に返させる (#94)。それ以外はヒープに読み込む。
		var (
			cached    []byte
			inMemory  bool
			accelPath string
			onDisk    bool
		)
		if useAccel {
			cached, inMemory = imageDataStore.getMemory(input.ImageID)
			if !inMemory {
				accelPath, onDisk = imageDiskAccelPath(input.ImageID)
			}
		} else {
			cached, inMemory = imageDataStore.get(input.ImageID)
		}
		hasData := inMemory || onDisk
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
		if onDisk {
			// nginx は X-Accel-Redirect を受けると上流の本文と状態を捨て、internal location の実ファイルを
			// sendfile で返す。Content-Type と Cache-Control (no-store、冒頭で設定済み) は上流の値が引き継がれる
			// (手元の nginx で確認)。
			if exp <= time.Now().Unix() {
				return NewAPIError(http.StatusForbidden, "image_forbidden")
			}
			c.Response().Header().Set("Content-Type", object.MediaType)
			c.Response().Header().Set("X-Accel-Redirect", accelPath)
			return c.NoContent(http.StatusOK)
		}
		if inMemory {
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
