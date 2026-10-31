package isureal

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

func deleteAccountHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var result string
		if err := h.DB.GetContext(c.Request().Context(), &result, `SELECT isureal_delete_account($1)`, GetSession(c).UserID); err != nil {
			return err
		}
		if result != "ok" {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		return c.NoContent(http.StatusNoContent)
	}
}
