package isureal

import (
	"context"
	"log"
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

// 通知の INSERT (期間のコミット後の 2 つ目のトランザクション) の制限。リクエストの context からは
// 切り離す: 期間はもう開いているので、管理クライアントが切断しても行は入れ切る。
const (
	notificationInsertTimeout  = 10 * time.Second
	notificationInsertAttempts = 3
)

type openedPostingWindow struct {
	StartsAt       time.Time `db:"starts_at"`
	OnTimeDeadline time.Time `db:"on_time_deadline"`
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
		// 開始処理は 2 つのトランザクションに分ける (#28)。
		//   1. 競合確認、直前の期間の終了、新しい期間の INSERT → コミット。この時点で期間が見え、
		//      投稿を受け付け、watcher (notification_api.go) が SSE を配り始める。
		//   2. notifications 表への INSERT (リージョンの active な利用者 1 人 1 行、数千行・数百 ms)。
		// SSE は notifications 表を読まず、期間と利用者から同じ id を組み立てる (#27) ので、
		// 2 を待たせる必要がない。1 のトランザクションが短くなる分、直前の期間の行ロック
		// (create_post の FOR SHARE と競合する) を持つ時間も短くなる。
		tx, err := h.DB.BeginTxx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
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
		// 先に FOR UPDATE で行ロックを取る CTE (MATERIALIZED: 本体に畳み込まれない) にしている。
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
   )
   INSERT INTO posting_windows (id, region, local_date, starts_at, on_time_deadline)
   SELECT $2, $1, $3, starts_at, starts_at + $4::bigint * interval '1 second' FROM starts
   RETURNING starts_at, on_time_deadline
  `, region, windowID, localDate, seconds); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		// 通知の id は SSE (notification_api.go) が投稿期間と利用者から同じ関数で組み立てる。
		// 期間はもうコミットされているので、失敗しても期間は開いたまま (再送された開始要求は 409)。
		// 同じ (期間, 利用者) の行が残っていても ON CONFLICT DO NOTHING で再実行できる。
		//
		// 応答は通知の INSERT を待たない。notifications 表は実行中に誰も読まない (SSE は期間と利用者から
		// 組み立てる #27) ので、応答を待たせても得るものが無い。一方ベンチは開始の応答を待ってから
		// そのリージョンの 1 日を進めるので、待たせた分だけ 1 日の開始が遅れる。ベンチ中の通知 INSERT は
		// 平均 760ms・最大 7.5 秒で、アジア最終日の開始が 8.4 秒遅れ、グランプリ (20 点) の達成が
		// 420 人中 12 人に留まっていた (#97)。
		go func() {
			if err := insertPostingWindowNotifications(ctx, h.DB, region, windowID, opened.StartsAt); err != nil {
				log.Printf("failed to insert notifications for window %s: %v", windowID, err)
			}
		}()
		return c.JSON(http.StatusCreated, openPostingWindowResponse{
			ID: windowID, Region: region, LocalDate: localDate.Format(time.DateOnly),
			StartsAt: opened.StartsAt, OnTimeDeadline: opened.OnTimeDeadline,
		})
	}
}

func insertPostingWindowNotifications(ctx context.Context, db sqlx.ExecerContext, region, windowID string, startsAt time.Time) error {
	return retryDetached(ctx, notificationInsertTimeout, notificationInsertAttempts, func(ctx context.Context, attempt int, last error) error {
		if last != nil {
			log.Printf("retrying notification insert for window %s (attempt %d): %v", windowID, attempt, last)
		}
		_, err := db.ExecContext(ctx, `
   INSERT INTO notifications (id, user_id, posting_window_id, created_at)
   SELECT isureal_notification_id($2::text, users.id, $3::timestamptz), users.id, $2, $3
   FROM users
   WHERE region = $1 AND status = 'active'
   ON CONFLICT DO NOTHING
  `, region, windowID, startsAt)
		return err
	})
}

// retryDetached は fn を、親の取り消しから切り離した timeout 付きの context で最大 attempts 回試す。
// fn が nil を返したら成功。timeout を使い切ったらその時点の誤りを返す。
func retryDetached(parent context.Context, timeout time.Duration, attempts int, fn func(ctx context.Context, attempt int, last error) error) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), timeout)
	defer cancel()
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err = fn(ctx, attempt, err); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return err
		}
	}
	return err
}
