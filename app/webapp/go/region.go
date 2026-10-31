package isureal

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

func RegionMiddleware(h *HandlerContext) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			session := GetSession(c)
			if session == nil {
				return NewAPIError(http.StatusUnauthorized, "invalid_session")
			}
			if _, deferred := deferredSession(c); deferred {
				// リージョンは書き込みの SQL の中で確認する (callWithSession)。
				return next(c)
			}
			if session.Region != h.Config.Region {
				return NewAPIError(http.StatusForbidden, "region_mismatch")
			}
			return next(c)
		}
	}
}
