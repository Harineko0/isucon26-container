package isureal

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type openPostingWindowRequest struct {
	Region        string `json:"region"`
	LocalDate     string `json:"localDate"`
	OnTimeSeconds *int64 `json:"onTimeSeconds"`
}

type openPostingWindowResponse struct {
	ID             string    `json:"id"`
	Region         string    `json:"region"`
	LocalDate      string    `json:"localDate"`
	StartsAt       time.Time `json:"startsAt"`
	OnTimeDeadline time.Time `json:"onTimeDeadline"`
}

func openPostingWindowHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input openPostingWindowRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		region := input.Region
		localDate, err := time.Parse(time.DateOnly, input.LocalDate)
		if !validRegion(region) || err != nil {
			return NewAPIError(http.StatusBadRequest, "invalid_posting_window")
		}
		seconds := int64(24 * 60 * 60)
		if input.OnTimeSeconds != nil {
			seconds = *input.OnTimeSeconds
		}
		ctx := c.Request().Context()
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// 通知の対象は INSERT ... SELECT の 1 文のスナップショットで決まるので、users のテーブルロックは取らない
		// (同時に行われた地域変更・登録は、この投稿期間の開始より後に起きた場合と同じ結果になる)。
		var conflict bool
		if err := tx.GetContext(ctx, &conflict, `SELECT EXISTS(SELECT 1 FROM posting_windows WHERE region = $1 AND local_date >= $2)`, region, localDate); err != nil {
			return err
		}
		if conflict {
			return NewAPIError(http.StatusConflict, "posting_window_conflict")
		}
		// 直前の投稿期間の終了時刻は、投稿(create_post の FOR SHARE)が終わるのを待って行ロックを取った後に決める。
		// 待つ前の時刻で閉じると、待っている間に旧期間へ入った投稿の posted_at が ended_at より後になる。
		// UPDATE ... SET ended_at = clock_timestamp() だけだと値はロック待ちの前に計算されるので、
		// 先に FOR UPDATE で行ロックを取る CTE にしている。開いている期間が無ければその時点の時刻を使う。
		// 新しい期間の starts_at と通知の created_at もこの時刻にそろえる。
		var startsAt time.Time
		if err := tx.GetContext(ctx, &startsAt, `
   WITH locked AS (
     SELECT id FROM posting_windows WHERE region = $1 AND ended_at IS NULL FOR UPDATE
   ), ended AS (
     UPDATE posting_windows w SET ended_at = clock_timestamp() FROM locked WHERE w.id = locked.id
     RETURNING w.ended_at
   )
   SELECT COALESCE((SELECT max(ended_at) FROM ended), clock_timestamp())
  `, region); err != nil {
			return err
		}
		windowID := uuid.NewString()
		if _, err := tx.ExecContext(ctx, `
   INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline)
   VALUES ($1, $2, $3, $4, $5)
  `, windowID, region, localDate, startsAt, startsAt.Add(time.Duration(seconds)*time.Second)); err != nil {
			return err
		}
		// 通知の id は SSE (notification_api.go) が投稿期間と利用者から同じ関数で組み立てる。
		if _, err := tx.ExecContext(ctx, `
   INSERT INTO notifications (id, user_id, posting_window_id, created_at)
   SELECT isureal_notification_id($2::text, users.id, $3::timestamptz), users.id, $2, $3
   FROM users
   WHERE region = $1 AND status = 'active'
  `, region, windowID, startsAt); err != nil {
			return err
		}
		window, err := getPostingWindow(ctx, tx, windowID)
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, openPostingWindowResponse{
			ID: window.ID, Region: window.Region, LocalDate: window.LocalDate.Format(time.DateOnly),
			StartsAt: window.StartsAt, OnTimeDeadline: window.OnTimeDeadline,
		})
	}
}
