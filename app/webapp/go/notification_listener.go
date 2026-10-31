package isureal

import (
	"context"
	"crypto/md5"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
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
	// A single snapshot decides both whether the window is still open and
	// which subscribers can receive it. Normally every subscriber qualifies,
	// in which case the result is just the common timestamp and ID prefix.
	var startsAt time.Time
	var prefix string
	var exceptions []byte
	err := db.QueryRowxContext(ctx, `
    SELECT w.starts_at, left(isureal_notification_id(w.id, '', w.starts_at), 13),
           (SELECT json_object_agg(ids.id, u.id IS NOT NULL)
            FROM unnest($2::text[]) ids(id)
            LEFT JOIN users u ON u.id = ids.id
            WHERE u.id IS NULL OR u.status <> 'active' OR u.region <> w.region)
    FROM posting_windows w
    WHERE w.id = $1 AND w.region = $3 AND w.ended_at IS NULL
   `, windowID, userIDs, region).Scan(&startsAt, &prefix, &exceptions)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// An existing but inactive/moved user is disconnected. A missing user
	// produced no row in the original join and is handled by checkUsers.
	var excluded map[string]bool
	if len(exceptions) != 0 {
		if err := json.Unmarshal(exceptions, &excluded); err != nil {
			return nil, err
		}
	}
	rows := make([]windowNotification, 0, len(userIDs))
	for _, id := range userIDs {
		if exists, skip := excluded[id]; skip {
			if exists {
				rows = append(rows, windowNotification{UserID: id, kick: true})
			}
			continue
		}
		rows = append(rows, windowNotification{
			UserID: id, ID: windowNotificationID(prefix, windowID, id), CreatedAt: startsAt,
		})
	}
	return rows, nil
}

// Match the suffix of isureal_notification_id, including its variant derived
// from the low two bits of digest byte 2. PostgreSQL supplies the formatted
// timestamp prefix, so its rounding and lpad behavior are preserved.
func windowNotificationID(prefix, windowID, userID string) string {
	digest := md5.Sum([]byte(windowID + ":" + userID))
	var h [md5.Size * 2]byte
	hex.Encode(h[:], digest[:])
	variant := "89ab"[digest[2]&3]
	return prefix + "-4" + string(h[:3]) + "-" + string(variant) + string(h[4:7]) + "-" + string(h[7:19])
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
