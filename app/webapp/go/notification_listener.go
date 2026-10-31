package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
)

const windowChannelPrefix = "isureal_window_"

type windowReader interface {
	activeWindow(ctx context.Context, region string) (string, error)
	notifications(ctx context.Context, region, windowID string, userIDs []string) ([]windowNotification, error)
}

type windowDB struct{ sqlx.QueryerContext }

func (db windowDB) activeWindow(ctx context.Context, region string) (string, error) {
	var id string
	err := sqlx.GetContext(ctx, db, &id, `SELECT id FROM posting_windows WHERE region = $1 AND ended_at IS NULL`, region)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

func (db windowDB) notifications(ctx context.Context, region, windowID string, userIDs []string) ([]windowNotification, error) {
	var rows []windowNotification
	err := sqlx.SelectContext(ctx, db, &rows, `
    SELECT u.id AS user_id, isureal_notification_id(w.id, u.id, w.starts_at) AS id, w.starts_at AS created_at,
           u.status = 'active' AS user_active, u.region AS user_region
    FROM posting_windows w JOIN users u ON u.id = ANY($2::text[])
    WHERE w.id = $1 AND w.region = $3 AND w.ended_at IS NULL
   `, windowID, userIDs, region)
	return rows, err
}

func (w *postingWindowWatcher) runWindowLoop(ctx context.Context, db *sqlx.DB) {
	for ctx.Err() == nil {
		err := w.listenForWindows(ctx, db)
		if ctx.Err() != nil {
			return
		}
		log.Printf("posting window listener: %v", err)

		// LISTEN の失敗中も、通常の読み取りができる限り有効期間に追従する。
		checkCtx, cancel := context.WithTimeout(ctx, notificationQueryTimeout)
		err = w.checkWindow(checkCtx, windowDB{db}, "")
		cancel()
		if err != nil {
			log.Printf("failed to reconcile posting window: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(notificationWindowResyncInterval):
		}
	}
}

func (w *postingWindowWatcher) listenForWindows(ctx context.Context, db *sqlx.DB) error {
	setupCtx, cancel := context.WithTimeout(ctx, notificationQueryTimeout)
	defer cancel()
	conn, err := db.Connx(setupCtx)
	if err != nil {
		return err
	}
	// LISTEN はセッションに残るので、切断・再接続時にはこの物理接続をプールへ戻さない。
	defer conn.Close()
	defer conn.Raw(func(any) error { return driver.ErrBadConn })

	channel := pgx.Identifier{windowChannelPrefix + w.region}.Sanitize()
	if _, err := conn.ExecContext(setupCtx, "LISTEN "+channel); err != nil {
		return err
	}
	cancel()

	// LISTEN を確定してから現在の期間を読む。この間のコミットは接続にキューされる。
	// 同じ接続で対象者を確認するので、通知を受けてから共有プールに並び直す必要がない。
	hint := ""
	for {
		checkCtx, cancel := context.WithTimeout(ctx, notificationQueryTimeout)
		err := w.checkWindow(checkCtx, windowDB{conn}, hint)
		cancel()
		if err != nil {
			return err
		}

		hint = ""
		waitCtx, cancel := context.WithTimeout(ctx, notificationWindowResyncInterval)
		err = conn.Raw(func(raw any) error {
			n, err := raw.(*stdlib.Conn).Conn().WaitForNotification(waitCtx)
			if err == nil {
				hint = n.Payload
			}
			return err
		})
		cancel()
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
}
