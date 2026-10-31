package isureal

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
)

// ipBansDriver は ip_bans の SELECT に、その時点の rows を返すだけのドライバ。
type ipBansDriver struct{ rows *atomic.Pointer[map[string]time.Time] }

func (d ipBansDriver) Open(string) (driver.Conn, error) { return ipBansConn(d), nil }

type ipBansConn ipBansDriver

func (ipBansConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (ipBansConn) Close() error                        { return nil }
func (ipBansConn) Begin() (driver.Tx, error)           { return nil, errors.New("not supported") }
func (c ipBansConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	r := &ipBansRows{}
	for ip, until := range *c.rows.Load() {
		r.rows = append(r.rows, [2]driver.Value{ip, until})
	}
	return r, nil
}

type ipBansRows struct{ rows [][2]driver.Value }

func (*ipBansRows) Columns() []string { return []string{"ip", "banned_until"} }
func (*ipBansRows) Close() error      { return nil }
func (r *ipBansRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	dest[0], dest[1] = r.rows[0][0], r.rows[0][1]
	r.rows = r.rows[1:]
	return nil
}

var ipBansRowsForTest atomic.Pointer[map[string]time.Time]

func init() { sql.Register("isureal-ipbans", ipBansDriver{rows: &ipBansRowsForTest}) }

// initialize の TRUNCATE のあと reloadNow を呼ぶと、tick を待たずに前回のベンチの BAN が外れる (#41)。
func TestIPBanReloadNowDropsTruncatedBans(t *testing.T) {
	raw, err := sql.Open("isureal-ipbans", "")
	if err != nil {
		t.Fatal(err)
	}
	db := sqlx.NewDb(raw, "pgx")
	defer db.Close()

	l := newIPBanList()
	banned := map[string]time.Time{"1.2.3.4": time.Now().Add(time.Minute)}
	ipBansRowsForTest.Store(&banned)
	if err := l.reloadNow(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if !l.bannedCached("1.2.3.4", time.Now()) {
		t.Fatal("ban from ip_bans not loaded")
	}

	ipBansRowsForTest.Store(&map[string]time.Time{}) // TRUNCATE ip_bans
	if err := l.reloadNow(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if l.bannedCached("1.2.3.4", time.Now()) {
		t.Fatal("banned after ip_bans was truncated and reloaded")
	}
}

// TRUNCATE より前に読み始めた reloadLoop の結果が、あとから届いても reloadNow の結果を戻さない。
func TestIPBanStoreKeepsNewerSnapshot(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	stale := &ipBanSnapshot{loadedAt: t0, bans: map[string]time.Time{"1.2.3.4": t0.Add(time.Hour)}}
	fresh := &ipBanSnapshot{loadedAt: t0.Add(10 * time.Millisecond), bans: map[string]time.Time{}}

	l := newIPBanList()
	if !l.store(fresh) {
		t.Fatal("first snapshot not stored")
	}
	if l.store(stale) {
		t.Fatal("older snapshot replaced a newer one")
	}
	if l.bannedCached("1.2.3.4", t0) {
		t.Fatal("stale ban came back")
	}
	newer := &ipBanSnapshot{loadedAt: t0.Add(time.Second), bans: map[string]time.Time{"5.6.7.8": t0.Add(time.Hour)}}
	if !l.store(newer) || !l.bannedCached("5.6.7.8", t0) {
		t.Fatal("newer snapshot not stored")
	}
}

// initialize は、ほかのリージョンの読み直し (次の tick + EU/US と asia2 の往復 約110ms) が
// 終わるまで待ってから返す。すでに経っていれば待たない。
func TestIPBanPeerWait(t *testing.T) {
	t.Parallel()
	const crossRegionRTT = 110 * time.Millisecond
	if ipBanPeerSettle < ipBanReloadInterval+crossRegionRTT {
		t.Fatalf("ipBanPeerSettle = %v, shorter than one tick + cross-region RTT (%v)", ipBanPeerSettle, ipBanReloadInterval+crossRegionRTT)
	}
	t0 := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		elapsed time.Duration
		want    time.Duration
	}{
		{"just reset", 0, ipBanPeerSettle},
		{"partly elapsed", 150 * time.Millisecond, ipBanPeerSettle - 150*time.Millisecond},
		{"already settled", ipBanPeerSettle, 0},
		{"long after", 5 * time.Second, 0},
	}
	for _, tt := range tests {
		if got := ipBanPeerWait(t0, t0.Add(tt.elapsed)); got != tt.want {
			t.Errorf("%s: ipBanPeerWait = %v, want %v", tt.name, got, tt.want)
		}
	}
}
