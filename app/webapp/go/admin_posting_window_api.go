package isureal

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
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

type openedPostingWindow struct {
	StartsAt       time.Time `db:"starts_at"`
	OnTimeDeadline time.Time `db:"on_time_deadline"`
}

func registerAdminPostingWindow(e *echo.Echo, control *sqlx.DB) {
	e.POST("/api/admin/posting-window", openPostingWindowHandler(control), checkAdminSession(control, true))
}

func openPostingWindowHandler(control *sqlx.DB) echo.HandlerFunc {
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
		// The recipients and the window commit together. A later region change or
		// process restart cannot change the audience of an already opened window.
		tx, err := control.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// リージョンごとの advisory lock を排他モードで、トランザクションの最初に取る (#79)。
		// 投稿は isureal_create_post の中で同じ鍵を共有モードで取るので、ここで待つのは要求の時点で
		// 鍵を持っている投稿 (画像の INSERT を済ませた後の残りの数文) だけで、以後の投稿はこの
		// トランザクションのコミットまで待つ。待ち行列は公平で、後から来た共有ロック要求に追い越されない
		// (以前の posting_windows 行の FOR SHARE は、行が共有ロックだけで持たれていると待っている FOR UPDATE が
		// あっても取れたので、投稿が途切れないと切替が 1 秒ほど待ち、同じ 1 行への共有ロックの重なりが MultiXact も作っていた)。
		// 同じリージョンの開始要求どうしもこの鍵で直列化されるので、下の重複確認と INSERT の間に別の開始が
		// 入ることはない。別のリージョンの開始・投稿とは独立に進む。
		// 以前ここで取っていた LOCK TABLE posting_windows IN EXCLUSIVE MODE は取らない: 投稿は共有の advisory lock を
		// 持ったまま posts の INSERT の FK 検査で posting_windows の ROW SHARE を待ち、こちらは表ロックを持ったまま
		// advisory lock を待つので、deadlock になる (job 24690 の次の run で asia / america の開始が 40P01)。
		// ACCESS SHARE (ふつうの SELECT: フィード、現在の期間、SSE の watcher) は何にも待たされない。
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(isureal_region_lock_key($1))`, region); err != nil {
			return err
		}
		var conflict bool
		if err := tx.GetContext(ctx, &conflict, `SELECT EXISTS(SELECT 1 FROM posting_windows WHERE region = $1 AND local_date >= $2)`, region, localDate); err != nil {
			return err
		}
		if conflict {
			return NewAPIError(http.StatusConflict, "posting_window_conflict")
		}
		// 直前の投稿期間の終了時刻は、上で advisory lock を取った後 (進行中の投稿が終わった後) に決める。
		// 待つ前の時刻で閉じると、待っている間に旧期間へ入った投稿の posted_at が ended_at より後になる。
		// ended_at の値 (clock_timestamp()) はロック取得後のこの文で計算される。開いている期間の行は FOR UPDATE で
		// 取ったまま (MATERIALIZED: 本体に畳み込まれない) にして、同じリージョンの切替どうしも行で直列化する。
		// 開いている期間が無ければその時点の時刻を使う。新しい期間の starts_at と通知の created_at も
		// この時刻にそろえる。新しい期間の INSERT も同じ文で行い、往復を 1 回にする。
		windowID := uuid.NewString()
		var opened openedPostingWindow
		if err := tx.GetContext(ctx, &opened, `
   WITH locked AS MATERIALIZED (
     SELECT id FROM posting_windows WHERE region = $1 AND ended_at IS NULL FOR UPDATE
   ), ended AS (
     UPDATE posting_windows w SET ended_at = clock_timestamp() FROM locked WHERE w.id = locked.id
     RETURNING w.ended_at
   ), starts AS (
     SELECT COALESCE((SELECT max(ended_at) FROM ended), clock_timestamp()) AS starts_at
   ), opened AS (
     INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline, notification_recipients)
     SELECT $2, $1, $3, starts_at, starts_at + $4::bigint * interval '1 second',
            ARRAY(SELECT id::text FROM users WHERE region = $1 AND status = 'active')
     FROM starts
     RETURNING id, region, starts_at, on_time_deadline
   )
   SELECT starts_at, on_time_deadline FROM opened,
          LATERAL pg_notify('`+windowChannelPrefix+`' || opened.region, opened.id)
  `, region, windowID, localDate, seconds); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return c.JSON(http.StatusCreated, openPostingWindowResponse{
			ID: windowID, Region: region, LocalDate: localDate.Format(time.DateOnly),
			StartsAt: opened.StartsAt, OnTimeDeadline: opened.OnTimeDeadline,
		})
	}
}
