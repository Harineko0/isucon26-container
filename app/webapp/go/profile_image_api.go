package isureal

import (
	"bytes"
	"net/http"

	"example.com/isureal-company/isureal/internal/jpegcheck"
	"github.com/labstack/echo/v5"
)

func putProfileImageHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if c.Request().Header.Get("Content-Type") != "image/jpeg" {
			return NewAPIError(http.StatusUnsupportedMediaType, "unsupported_media_type")
		}
		// nginx は本文をバッファして Content-Length 付きで渡す (上限 32m) ので、
		// その長さを 1 回だけ確保して読む (#93)。
		data, err := readUpload(c.Request().Body, c.Request().ContentLength)
		if err != nil {
			return err
		}
		// Validate first: the abuse flag must be set exactly when
		// image/jpeg.Decode would fail, including for oversized images.
		width, height, err := jpegcheck.Validate(bytes.NewReader(data))
		if err != nil {
			c.Set("abuse", "invalid_image")
			return NewAPIError(http.StatusBadRequest, "invalid_profile_image")
		}
		if width > 512 || height > 512 {
			return NewAPIError(http.StatusBadRequest, "invalid_profile_image")
		}

		imageID := newTimeID()
		// セッションの確認と同じ 1 文で呼ぶ (#17)。このルートにはリージョンの確認が無い。
		var row struct {
			SessRegion *string `db:"sess_region"`
			Result     *string `db:"result"`
		}
		if err := callWithSessionAnyRegion(c, h, &row, `(SELECT isureal_put_profile_image({user}, $1, $2) AS result)`, imageID, data); err != nil {
			return err
		}
		if row.Result == nil || *row.Result != "ok" {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		imageDataStore.put(imageID, data)
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
