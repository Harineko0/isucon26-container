package isureal

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
)

// #17: 残っていた「セッション確認 → 書き込み」の 2 往復を、callWithSession / callWithSessionAnyRegion の 1 文にする。
func init() {
	for _, route := range []string{
		"POST /api/me/account-deletion",
		"POST /api/friend-requests/:requestId/:action",
	} {
		deferredSessionRoutes[route] = true
	}
	for route := range anyRegionDeferredRoutes {
		deferredSessionRoutes[route] = true
	}
}

// anyRegionDeferredRoutes は deferred のうち RegionMiddleware の無いルート (callWithSessionAnyRegion を使う)。
// 参照実装はこれらでリージョンを確認しないので、リージョンの違う利用者でも 403 にせず、ハンドラの結果 (400 など) を返す。
var anyRegionDeferredRoutes = map[string]bool{
	"POST /api/auth/logout":        true,
	"PATCH /api/me":                true,
	"PUT /api/me/profile-image":    true,
	"DELETE /api/me/profile-image": true,
}

// callWithSessionAnyRegion は callWithSession と同じく `call` をセッションの確認と同じ 1 文で実行するが、
// リージョンは確認しない。RegionMiddleware の無いルート (logout、PATCH /api/me、profile-image) 用で、
// PATCH /api/me はリージョンを変える操作でもある。セッションが無効なら 401。
// スカラーを返す関数は `(SELECT f({user}, $1) AS result)` の形で渡す: 素の `FROM f(...) r` だと列名が r になり、
// deferred の LATERAL 形 (列名が関数名) と一致しないため、両方の経路で result にそろえる
// (別名の無い FROM の副問い合わせは PostgreSQL 16 から書ける)。
func callWithSessionAnyRegion(c *echo.Context, h *HandlerContext, dest any, call string, args ...any) error {
	ctx := c.Request().Context()
	tokenHash, deferred := deferredSession(c)
	if !deferred {
		args = append(args, GetSession(c).UserID)
		query := `SELECT NULL::text AS sess_region, r.* FROM ` + strings.ReplaceAll(call, "{user}", fmt.Sprintf("$%d", len(args))) + ` r`
		return h.DB.GetContext(ctx, dest, query, args...)
	}
	args = append(args, tokenHash)
	query := fmt.Sprintf(`
		SELECT s.sess_region, r.* FROM (
			SELECT s.user_id AS sess_user_id, u.region AS sess_region
			FROM login_sessions s JOIN users u ON u.id = s.user_id
			WHERE s.token_hash = $%d AND s.active = true AND u.status = 'active'
		) s LEFT JOIN LATERAL (SELECT * FROM %s OFFSET 0) r ON true`,
		len(args), strings.ReplaceAll(call, "{user}", "s.sess_user_id"))
	err := h.DB.GetContext(ctx, dest, query, args...)
	if errors.Is(err, sql.ErrNoRows) {
		c.Set(deferredSessionCheckedKey, true)
		knownSessions.Delete(tokenHash)
		c.Set("session", nil)
		return NewAPIError(http.StatusUnauthorized, "invalid_session")
	}
	if err == nil {
		// セッションはこの文で確認済み。この後のハンドラのエラー (404 など) をそのまま返す。
		c.Set(deferredSessionCheckedKey, true)
	}
	return err
}
