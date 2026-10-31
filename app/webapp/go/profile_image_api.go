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

		ctx := c.Request().Context()
		imageID := newImageID(h.Config.Region)
		// 本体はこのサーバーのディスクに fsync してから、DB にはメタデータだけを入れる (image_store.go)。
		dbData, storedRegion := storeUploadedImage(imageID, data, h.Config.Region)
		var result string
		if err := h.DB.GetContext(ctx, &result, `SELECT isureal_put_profile_image($1, $2, $3, $4)`, GetSession(c).UserID, imageID, dbData, storedRegion); err != nil {
			return err
		}
		if result != "ok" {
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
		var result string
		if err := h.DB.GetContext(c.Request().Context(), &result, `SELECT isureal_delete_profile_image($1)`, GetSession(c).UserID); err != nil {
			return err
		}
		if result != "ok" {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		return c.NoContent(http.StatusNoContent)
	}
}
