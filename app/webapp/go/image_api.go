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

// imageAccelRegions は、ディスクにある画像を Go のヒープに載せず X-Accel-Redirect で nginx に返させる (#94)
// リージョン。PR #130 の初回ベンチ (job 21475) では、EU/US の画像取得は 0.235/0.206 → 0.175/0.178 秒と速くなった
// 一方、asia1 は 0.161 → 0.265 秒に遅くなり iowait が 22% に上がった (16GB の imgcache に対してページキャッシュが
// 小さく、ヒープのキャッシュが外れた分がディスク読みになったとみられる)。asia はヒープのキャッシュに戻す。
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
		hint, hinted := loadImageHint(input.ImageID)
		if hinted && hint.key != nil && !hmac.Equal(sig, imageSignature(hint.key, input.ImageID, exp)) {
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
		// 署名鍵・閲覧可否・(未キャッシュなら)本体を 1 往復で取る。削除状態は毎回確認する。
		var object struct {
			SecretKeyHash string    `db:"secret_key_hash"`
			MediaType     string    `db:"media_type"`
			Data          []byte    `db:"data"`
			Allowed       bool      `db:"allowed"`
			CreatedAt     time.Time `db:"created_at"`
		}
		// 画像の created_at は不変なので、手がかり (imageCreatedAt) があれば created_at の範囲も付けてパーティションを絞る。
		imageQuery := `
   SELECT u.secret_key_hash, i.media_type, i.created_at, CASE WHEN $2 THEN NULL ELSE i.data END AS data, COALESCE(
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
		if hinted {
			// 範囲の両端は STABLE な式 (timestamptz ± interval) にして、プラン時ではなく実行開始時に刈り込ませる。
			// 定数 (= $3) のままだとプラン時に 1 パーティションへ刈り込めるカスタムプランの方が安く見えて
			// 汎用プランに切り替わらず、毎回プランニング (実測 ~1ms) が走る。この形なら 5 回目以降は汎用プラン
			// (プランニング ~0.05ms、実行開始時に 35 パーティションを除外) になる。
			err = conn.GetContext(ctx, &object, imageQuery+` AND i.created_at BETWEEN $3::timestamptz - interval '1 hour' AND $3::timestamptz + interval '0 seconds'`, input.ImageID, hasData, hint.createdAt)
			hinted = err == nil && hint.key != nil
			if errors.Is(err, sql.ErrNoRows) {
				// 手がかりが外れていた場合に備えて id だけで引き直す。
				err = conn.GetContext(ctx, &object, imageQuery, input.ImageID, hasData)
			}
		} else {
			err = conn.GetContext(ctx, &object, imageQuery, input.ImageID, hasData)
		}
		conn.Close()
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
		if !hinted {
			rememberImage(input.ImageID, object.CreatedAt, key)
		}
		if !hmac.Equal(sig, imageSignature(key, input.ImageID, exp)) {
			c.Set("abuse", "invalid_signature")
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if !object.Allowed {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if exp <= time.Now().Unix() {
			return NewAPIError(http.StatusForbidden, "image_forbidden")
		}
		if inMemory {
			return c.Blob(http.StatusOK, object.MediaType, cached)
		}
		if onDisk {
			// nginx は X-Accel-Redirect を受けると上流の本文と状態を捨て、internal location の実ファイルを
			// sendfile で返す。Content-Type と Cache-Control (no-store、冒頭で設定済み) は上流の値が引き継がれる。
			c.Response().Header().Set("Content-Type", object.MediaType)
			c.Response().Header().Set("X-Accel-Redirect", accelPath)
			return c.NoContent(http.StatusOK)
		}
		imageDataStore.put(input.ImageID, object.Data)
		saveInitialImageToDisk(input.ImageID, object.CreatedAt, object.Data)
		return c.Blob(http.StatusOK, object.MediaType, object.Data)
	}
}
