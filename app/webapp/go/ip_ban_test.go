package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// noopDriver は接続を数えるためだけのドライバ (クエリは使わない)。
type noopDriver struct{}

func (noopDriver) Open(string) (driver.Conn, error) { return noopConn{}, nil }

type noopConn struct{}

func (noopConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (noopConn) Close() error                        { return nil }
func (noopConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }

func init() { sql.Register("isureal-noop", noopDriver{}) }

func TestIPBanListBannedCached(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	l := newIPBanList()
	if l.bannedCached("1.2.3.4", now) {
		t.Fatal("banned with no bans loaded")
	}
	l.snapshot.Store(&ipBanSnapshot{bans: map[string]time.Time{"1.2.3.4": now.Add(time.Minute)}})
	if !l.bannedCached("1.2.3.4", now) {
		t.Fatal("ban from ip_bans not applied")
	}
	if l.bannedCached("1.2.3.4", now.Add(time.Minute)) {
		t.Fatal("expired ip_bans row still applied")
	}
	l.abuse.observe("invalid_image", "5.6.7.8", now)
	l.abuse.observe("invalid_image", "5.6.7.8", now)
	if !l.bannedCached("5.6.7.8", now) {
		t.Fatal("in-process ban not applied")
	}
}

func TestConnUnlessBanned(t *testing.T) {
	t.Parallel()
	raw, err := sql.Open("isureal-noop", "")
	if err != nil {
		t.Fatal(err)
	}
	db := sqlx.NewDb(raw, "pgx")
	defer db.Close()

	l := newIPBanList()
	l.snapshot.Store(&ipBanSnapshot{bans: map[string]time.Time{"1.2.3.4": time.Now().Add(time.Minute)}})

	// BAN されていれば入口と同じ 403 ip_banned で、接続はプールに返っている。
	conn, err := l.connUnlessBanned(context.Background(), db, "1.2.3.4")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusForbidden || apiErr.Code != "ip_banned" {
		t.Fatalf("err = %v, want 403 ip_banned", err)
	}
	if conn != nil {
		t.Fatal("conn returned for a banned IP")
	}
	if inUse := db.Stats().InUse; inUse != 0 {
		t.Fatalf("connections in use = %d, want 0", inUse)
	}

	// BAN されていなければ接続を渡す。
	conn, err = l.connUnlessBanned(context.Background(), db, "5.6.7.8")
	if err != nil || conn == nil {
		t.Fatalf("conn = %v, err = %v, want a connection", conn, err)
	}
	if inUse := db.Stats().InUse; inUse != 1 {
		t.Fatalf("connections in use = %d, want 1", inUse)
	}
	conn.Close()
	if inUse := db.Stats().InUse; inUse != 0 {
		t.Fatalf("connections in use after Close = %d, want 0", inUse)
	}
}
