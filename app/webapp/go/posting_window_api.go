package isureal

import (
	"context"
	"net/http"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type currentPostingWindowResponse struct {
	ID             string    `json:"id"`
	Region         string    `json:"region"`
	LocalDate      string    `json:"localDate"`
	StartsAt       time.Time `json:"startsAt"`
	OnTimeDeadline time.Time `json:"onTimeDeadline"`
}

type getCurrentPostingWindowResponse struct {
	Window        *currentPostingWindowResponse `json:"window"`
	HasPosted     bool                          `json:"hasPosted"`
	CanPost       bool                          `json:"canPost"`
	DeletionCount int                           `json:"deletionCount"`
}

type currentPostingWindowState struct {
	TokenHash      string     `db:"token_hash"`
	SessRegion     string     `db:"sess_region"`
	ID             *string    `db:"id"`
	Region         *string    `db:"region"`
	LocalDate      *time.Time `db:"local_date"`
	StartsAt       *time.Time `db:"starts_at"`
	OnTimeDeadline *time.Time `db:"on_time_deadline"`
	Active         int        `db:"active"`
	Deleted        int        `db:"deleted"`
}

// 同じリクエスト群で開いている期間と日付を一度だけ読み、各利用者の投稿を
// posts(author_id, posting_window_id) で引く。同日なら他リージョンに投稿した分も数える。
func loadCurrentWindows(ctx context.Context, db sqlx.QueryerContext, region string, tokens []string) (map[string]currentPostingWindowState, error) {
	var rows []currentPostingWindowState
	err := sqlx.SelectContext(ctx, db, &rows, `
   WITH current_window AS MATERIALIZED (
     SELECT id, region, local_date, starts_at, on_time_deadline FROM posting_windows
     WHERE region = $2 AND ended_at IS NULL
   ), day_windows AS MATERIALIZED (
     SELECT w.id FROM posting_windows w JOIN current_window c ON c.local_date = w.local_date
   )
   SELECT s.token_hash, u.region AS sess_region, w.id, w.region, w.local_date, w.starts_at, w.on_time_deadline,
          n.active, n.deleted
   FROM login_sessions s JOIN users u ON u.id = s.user_id AND u.status = 'active'
   LEFT JOIN current_window w ON w.region = u.region
   CROSS JOIN LATERAL (
     SELECT count(*) FILTER (WHERE p.deleted_at IS NULL) AS active,
            count(*) FILTER (WHERE p.deleted_at IS NOT NULL) AS deleted
     FROM day_windows d CROSS JOIN LATERAL (
       SELECT p.deleted_at FROM posts p
       WHERE p.author_id = u.id AND p.posting_window_id = d.id OFFSET 0
     ) p WHERE u.region = $2
   ) n
   WHERE s.active AND s.token_hash = ANY($1::text[])
 `, tokens, region)
	if err != nil {
		return nil, err
	}
	states := make(map[string]currentPostingWindowState, len(rows))
	for _, row := range rows {
		states[row.TokenHash] = row
	}
	return states, nil
}

func getCurrentPostingWindowHandler(h *HandlerContext) echo.HandlerFunc {
	reader := &windowReads{load: func(ctx context.Context, tokens []string) (map[string]currentPostingWindowState, error) {
		return loadCurrentWindows(ctx, h.DB, h.Config.Region, tokens)
	}}
	return func(c *echo.Context) error {
		token := GetSession(c).TokenHash
		state, found, err := reader.get(c.Request().Context(), token)
		if err != nil {
			return err
		}
		if err := rejectDeferredSession(c, h, token, found, state.SessRegion); err != nil {
			return err
		}
		if state.ID == nil {
			return c.JSON(http.StatusOK, getCurrentPostingWindowResponse{})
		}
		return c.JSON(http.StatusOK, getCurrentPostingWindowResponse{
			Window: &currentPostingWindowResponse{
				ID: *state.ID, Region: *state.Region, LocalDate: state.LocalDate.Format(time.DateOnly),
				StartsAt: *state.StartsAt, OnTimeDeadline: *state.OnTimeDeadline,
			},
			HasPosted:     state.Active > 0,
			CanPost:       state.Active == 0 && state.Deleted < 2 && !time.Now().Before(*state.StartsAt),
			DeletionCount: state.Deleted,
		})
	}
}
