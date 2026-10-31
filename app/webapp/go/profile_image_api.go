package isureal

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"

	"example.com/isureal-company/isureal/internal/jpegcheck"
	"github.com/labstack/echo/v5"
)

// A missing SOI marker proves that a JPEG is invalid. For large bodies,
// drain the request before reporting the usual invalid_image, without keeping
// those bytes in memory. A short read or transport error is still an I/O error.
func readProfileUpload(body io.Reader, size int64) ([]byte, error) {
	if size <= 2<<20 || size > maxUploadBytes {
		return readUpload(body, size)
	}
	r := bufio.NewReader(body)
	marker, err := r.Peek(2)
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	// A streaming client can stop after the marker. Grow with received data
	// instead of reserving its entire Content-Length on arrival.
	var image bytes.Buffer
	var dst io.Writer = io.Discard
	if marker[0] == 0xff && marker[1] == 0xd8 {
		dst = &image
	}
	n, err := io.Copy(dst, io.LimitReader(r, maxUploadBytes+1))
	switch {
	case err != nil:
		return nil, err
	case n < size:
		return nil, io.ErrUnexpectedEOF
	case n > maxUploadBytes:
		return nil, errUploadTooLarge
	}
	// Cache accounting uses len(data); release any spare buffer capacity.
	// A discarded body returns nil and takes the usual invalid_image path.
	return bytes.Clone(image.Bytes()), nil
}

func putProfileImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if c.Request().Header.Get("Content-Type") != "image/jpeg" {
			return NewAPIError(http.StatusUnsupportedMediaType, "unsupported_media_type")
		}
		data, err := readProfileUpload(c.Request().Body, c.Request().ContentLength)
		if err != nil {
			return err
		}
		// Validate first: the abuse flag must be set exactly when
		// image/jpeg.Decode would fail, including for oversized images.
		width, height, err := jpegcheck.ValidateBytes(data)
		if err != nil {
			c.Set("abuse", "invalid_image")
			return NewAPIError(http.StatusBadRequest, "invalid_profile_image")
		}
		if width > 512 || height > 512 {
			return NewAPIError(http.StatusBadRequest, "invalid_profile_image")
		}

		imageID := newImageID(h.Config.Region)
		// 本体はこのサーバーのディスクに fsync してから、DB にはメタデータだけを入れる (image_store.go)。
		dbData, storedRegion := storeUploadedImage(imageID, data, h.Config.Region)
		// セッションの確認と同じ 1 文で呼ぶ (#17)。このルートにはリージョンの確認が無い。
		var row struct {
			SessRegion *string `db:"sess_region"`
			Result     *string `db:"result"`
		}
		if err := callWithSessionAnyRegion(c, h, &row, `(SELECT isureal_put_profile_image({user}, $1, $2, $3) AS result)`, imageID, dbData, storedRegion); err != nil {
			// コミット済みかどうか分からない (応答の読み取り中の切断など) ので、ファイルは消さない
			// (投稿・RealMoji と同じ。残っても id は使い回さないのでディスクを使うだけ)。
			return err
		}
		if row.Result == nil || *row.Result != "ok" {
			discardUploadedImage(imageID, storedRegion)
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		imageDataStore.put(imageID, data)
		enqueueImagePush(imageID, data, storedRegion, h.Config.Region)
		return c.NoContent(http.StatusNoContent)
	}
}

func deleteProfileImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var row struct {
			SessRegion *string `db:"sess_region"`
			Result     *string `db:"result"`
		}
		if err := callWithSessionAnyRegion(c, h, &row, `(SELECT isureal_delete_profile_image({user}) AS result)`); err != nil {
			return err
		}
		if row.Result == nil || *row.Result != "ok" {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		return c.NoContent(http.StatusNoContent)
	}
}
