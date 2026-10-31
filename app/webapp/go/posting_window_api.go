package isureal

import (
	"database/sql"
	"errors"
	"net/http"
	"time"

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

// 有効な投稿期間と、同じ日付の期間への自分の投稿数 (削除済みを含む)。u は利用者の行。
const currentPostingWindowFrom = `
   LEFT JOIN posting_windows w ON w.region = u.region AND w.ended_at IS NULL
   LEFT JOIN posting_windows posted_window ON posted_window.local_date = w.local_date
   LEFT JOIN posts p ON p.author_id = u.id AND p.posting_window_id = posted_window.id`

const currentPostingWindowColumns = `
   w.id, w.region, w.local_date, w.starts_at, w.on_time_deadline,
   count(p.id) FILTER (WHERE p.deleted_at IS NULL) AS active,
   count(p.id) FILTER (WHERE p.deleted_at IS NOT NULL) AS deleted`

type currentPostingWindowState struct {
	SessRegion     string     `db:"sess_region"`
	ID             *string    `db:"id"`
	Region         *string    `db:"region"`
	LocalDate      *time.Time `db:"local_date"`
	StartsAt       *time.Time `db:"starts_at"`
	OnTimeDeadline *time.Time `db:"on_time_deadline"`
	Active         int        `db:"active"`
	Deleted        int        `db:"deleted"`
}

func getCurrentPostingWindowHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		ctx := c.Request().Context()
		var state currentPostingWindowState
		if tokenHash, deferred := deferredSession(c); deferred {
			// セッションの確認 (401 → 403) と期間の取得を 1 文で行う。
			err := h.DB.GetContext(ctx, &state, `
   SELECT u.region AS sess_region,`+currentPostingWindowColumns+`
   FROM login_sessions s JOIN users u ON u.id = s.user_id`+currentPostingWindowFrom+`
   WHERE s.token_hash = $1 AND s.active = true AND u.status = 'active'
   GROUP BY u.id, w.id
  `, tokenHash)
			found := err == nil
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err := rejectDeferredSession(c, h, tokenHash, found, state.SessRegion); err != nil {
				return err
			}
		} else {
			err := h.DB.GetContext(ctx, &state, `
   SELECT u.region AS sess_region,`+currentPostingWindowColumns+`
   FROM users u`+currentPostingWindowFrom+`
   WHERE u.id = $1
   GROUP BY u.id, w.id
  `, GetSession(c).UserID)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
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
