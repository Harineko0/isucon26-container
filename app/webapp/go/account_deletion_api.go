package isureal

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

func deleteAccountHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		// セッションとリージョンの確認と同じ 1 文で呼ぶ (#17)。
		var row struct {
			SessRegion *string `db:"sess_region"`
			Result     *string `db:"result"`
		}
		if err := callWithSession(c, h, &row, func() *string { return row.SessRegion }, `(SELECT isureal_delete_account({user}) AS result)`); err != nil {
			return err
		}
		if row.Result == nil || *row.Result != "ok" {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		knownSessions.Delete(GetSession(c).TokenHash)
		return c.NoContent(http.StatusNoContent)
	}
}
