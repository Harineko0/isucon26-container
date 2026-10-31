package isureal

import (
	"context"
	"database/sql"
	"fmt"
	"github.com/jmoiron/sqlx"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestStatementCounterHot(t *testing.T) {
	s := &statementCounter{}
	for range 5 {
		s.record("SELECT a")
	}
	for range 7 {
		s.record("SELECT b")
	}
	for range 7 {
		s.record("SELECT 0")
	}
	s.record("SELECT rare")
	s.record("")
	got := s.hot(5, 10)
	if want := []string{"SELECT 0", "SELECT b", "SELECT a"}; !slices.Equal(got, want) {
		t.Fatalf("hot = %q, want %q", got, want)
	}
	if got := s.hot(5, 2); len(got) != 2 {
		t.Fatalf("limit not applied: %q", got)
	}
}

func TestWarmStatementsFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "prepared.json")
	if got := loadWarmStatementsFile(path, "s1"); got != nil {
		t.Fatalf("missing file = %q, want nil", got)
	}
	list := []string{"SELECT $1::int", "WITH x AS (SELECT 1) SELECT * FROM x"}
	if err := saveWarmStatementsFile(path, "s1", list); err != nil {
		t.Fatal(err)
	}
	if got := loadWarmStatementsFile(path, "s1"); !slices.Equal(got, list) {
		t.Fatalf("loaded %q, want %q", got, list)
	}
	// sql/ が変わった (別の指紋) なら使わず、ファイルも空になる (後で s1 に戻っても使わない)。
	if got := loadWarmStatementsFile(path, "s2"); got != nil {
		t.Fatalf("other schema = %q, want nil", got)
	}
	if got := loadWarmStatementsFile(path, "s1"); got != nil {
		t.Fatalf("after a schema change = %q, want nil", got)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadWarmStatementsFile(path, "s1"); got != nil {
		t.Fatalf("broken file = %q, want nil", got)
	}
}

func TestSchemaFingerprint(t *testing.T) {
	dir := t.TempDir()
	if _, err := schemaFingerprint(dir); err == nil {
		t.Fatal("empty dir: want error")
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("001-schema.sql", "CREATE TABLE a (id int);")
	write("init.sh", "#!/bin/bash")
	first, err := schemaFingerprint(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := schemaFingerprint(dir); again != first {
		t.Fatal("fingerprint is not stable")
	}
	write("004-functions.sql", "CREATE FUNCTION f() RETURNS int LANGUAGE sql AS 'SELECT 1';")
	if changed, _ := schemaFingerprint(dir); changed == first {
		t.Fatal("adding a sql file did not change the fingerprint")
	}
	if _, err := schemaFingerprint("../sql"); err != nil {
		t.Fatalf("repository sql/: %v", err)
	}
}

// roundTripCounter は接続の書き込み (pgconn は 1 往復につき 1 回まとめて送る) と Prepare を数える。
type roundTripCounter struct {
	writes   atomic.Int64
	prepares atomic.Int64
}

type countingNetConn struct {
	net.Conn
	c *roundTripCounter
}

func (n countingNetConn) Write(b []byte) (int, error) {
	n.c.writes.Add(1)
	return n.Conn.Write(b)
}

func (c *roundTripCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}
func (c *roundTripCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
func (c *roundTripCounter) TracePrepareStart(ctx context.Context, _ *pgx.Conn, _ pgx.TracePrepareStartData) context.Context {
	c.prepares.Add(1)
	return ctx
}
func (c *roundTripCounter) TracePrepareEnd(context.Context, *pgx.Conn, pgx.TracePrepareEndData) {}

func (c *roundTripCounter) reset() (writes, prepares int64) {
	return c.writes.Swap(0), c.prepares.Swap(0)
}

func countingConnConfig(t *testing.T, dsn string, counter *roundTripCounter) *pgx.ConnConfig {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	cfg.DialFunc = func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return countingNetConn{conn, counter}, nil
	}
	cfg.Tracer = counter
	return cfg
}

// TestWarmPreparedStatementsOnPostgres は、接続時のまとめての Prepare が 1 往復で、何も実行せず
// (書き込みの文も)、その後の実行が 1 往復 (Prepare なし) になり、結果が変わらないことを本物の PostgreSQL 16 で確かめる。
// ISUREAL_TEST_DSN (空の DB でよい) が無ければ飛ばす。手順は sql/tests/roundtrips.sh。
func TestWarmPreparedStatementsOnPostgres(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set (see app/webapp/sql/tests/roundtrips.sh)")
	}
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	table, seq := "warm_t_"+suffix, "warm_s_"+suffix

	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	for _, statement := range []string{
		`CREATE TABLE ` + table + ` (id int)`,
		`CREATE SEQUENCE ` + seq,
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP TABLE IF EXISTS `+table)
		_, _ = admin.Exec(context.Background(), `DROP SEQUENCE IF EXISTS `+seq)
	})

	nextval := `SELECT nextval('` + seq + `')`
	insert := `INSERT INTO ` + table + ` (id) VALUES ($1) RETURNING id`
	missing := `SELECT count(*) FROM no_such_table_` + suffix
	selectByID := `SELECT id FROM ` + table + ` WHERE id = $1`
	withQuery := `WITH x AS (SELECT $1::int AS v) SELECT v + 1 FROM x`
	list := []string{nextval, insert, missing, selectByID, withQuery}
	good := []string{nextval, insert, selectByID, withQuery}

	execute := func(t *testing.T, conn *pgx.Conn) {
		t.Helper()
		var n int64
		if err := conn.QueryRow(ctx, nextval).Scan(&n); err != nil || n != 1 {
			t.Fatalf("nextval = %d, %v; want 1", n, err)
		}
		var id int
		if err := conn.QueryRow(ctx, insert, 7).Scan(&id); err != nil || id != 7 {
			t.Fatalf("insert = %d, %v; want 7", id, err)
		}
		if err := conn.QueryRow(ctx, selectByID, 7).Scan(&id); err != nil || id != 7 {
			t.Fatalf("select = %d, %v; want 7", id, err)
		}
		if err := conn.QueryRow(ctx, withQuery, 2).Scan(&id); err != nil || id != 3 {
			t.Fatalf("with = %d, %v; want 3", id, err)
		}
	}
	resetData := func(t *testing.T) {
		t.Helper()
		if _, err := admin.Exec(ctx, `TRUNCATE `+table); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, `ALTER SEQUENCE `+seq+` RESTART`); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("without warm-up the first execution of each statement is 2 round trips", func(t *testing.T) {
		resetData(t)
		counter := &roundTripCounter{}
		conn, err := pgx.ConnectConfig(ctx, countingConnConfig(t, dsn, counter))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		counter.reset()
		execute(t, conn)
		if writes, prepares := counter.reset(); writes != 8 || prepares != 4 {
			t.Fatalf("writes = %d, prepares = %d; want 8 and 4 (Prepare + execute for each of 4)", writes, prepares)
		}
	})

	t.Run("warm-up prepares in 1 round trip, executes nothing, then each statement is 1 round trip", func(t *testing.T) {
		resetData(t)
		counter := &roundTripCounter{}
		conn, err := pgx.ConnectConfig(ctx, countingConnConfig(t, dsn, counter))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)

		counter.reset()
		warmed, bad, err := warmPreparedStatements(ctx, conn, good)
		if err != nil || warmed != len(good) || len(bad) != 0 {
			t.Fatalf("warmed = %d, bad = %q, err = %v", warmed, bad, err)
		}
		if writes, prepares := counter.reset(); writes != 1 || prepares != 0 {
			t.Fatalf("warm-up: writes = %d, prepares = %d; want 1 and 0", writes, prepares)
		}

		// 何も実行していない (nextval も INSERT も)。確認は別の接続で行う。
		var isCalled bool
		var rows int
		if err := admin.QueryRow(ctx, `SELECT is_called FROM `+seq).Scan(&isCalled); err != nil || isCalled {
			t.Fatalf("sequence is_called = %v, %v; want false", isCalled, err)
		}
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&rows); err != nil || rows != 0 {
			t.Fatalf("rows = %d, %v; want 0", rows, err)
		}

		counter.reset()
		execute(t, conn)
		if writes, prepares := counter.reset(); writes != 4 || prepares != 0 {
			t.Fatalf("after warm-up: writes = %d, prepares = %d; want 4 and 0", writes, prepares)
		}
	})

	t.Run("statements the server rejects are dropped and the rest are still prepared", func(t *testing.T) {
		resetData(t)
		counter := &roundTripCounter{}
		conn, err := pgx.ConnectConfig(ctx, countingConnConfig(t, dsn, counter))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		warmed, bad, err := warmPreparedStatements(ctx, conn, list)
		if err != nil || warmed != len(good) || !slices.Equal(bad, []string{missing}) {
			t.Fatalf("warmed = %d, bad = %q, err = %v", warmed, bad, err)
		}
		counter.reset()
		execute(t, conn)
		if writes, prepares := counter.reset(); writes != 4 || prepares != 0 {
			t.Fatalf("after warm-up: writes = %d, prepares = %d; want 4 and 0", writes, prepares)
		}
		if _, err := conn.Exec(ctx, missing); err == nil {
			t.Fatal("missing table: want error")
		}
	})

	t.Run("database/sql connections are warmed by AfterConnect", func(t *testing.T) {
		resetData(t)
		setWarmStatements(list)
		defer setWarmStatements(nil)
		counter := &roundTripCounter{}
		db := sql.OpenDB(stdlib.GetConnector(*countingConnConfig(t, dsn, counter), stdlib.OptionAfterConnect(afterConnectWarm)))
		defer db.Close()
		c, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if got := currentWarmStatements(); !slices.Equal(got, good) {
			t.Fatalf("warm list after a rejected statement = %q, want %q", got, good)
		}
		counter.reset()
		var id int
		if err := c.QueryRowContext(ctx, withQuery, 4).Scan(&id); err != nil || id != 5 {
			t.Fatalf("with = %d, %v; want 5", id, err)
		}
		if err := c.QueryRowContext(ctx, insert, 9).Scan(&id); err != nil || id != 9 {
			t.Fatalf("insert = %d, %v; want 9", id, err)
		}
		if writes, prepares := counter.reset(); writes != 2 || prepares != 0 {
			t.Fatalf("database/sql after warm-up: writes = %d, prepares = %d; want 2 and 0", writes, prepares)
		}
		var rows int
		if err := admin.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&rows); err != nil || rows != 1 {
			t.Fatalf("rows = %d, %v; want 1 (only the explicit INSERT)", rows, err)
		}
	})
}

// recordingTracer は実行した文面を数え (statementCounter)、Prepare の回数も数える。
type recordingTracer struct {
	statementCounter
	prepares atomic.Int64
}

func (r *recordingTracer) TracePrepareStart(ctx context.Context, _ *pgx.Conn, _ pgx.TracePrepareStartData) context.Context {
	r.prepares.Add(1)
	return ctx
}
func (r *recordingTracer) TracePrepareEnd(context.Context, *pgx.Conn, pgx.TracePrepareEndData) {}

// TestWarmedHandlersRespondTheSame は、#17 の PostgreSQL のシナリオ (GET/PATCH/DELETE posts、DELETE comments、
// 401/403/404 の順序、更新直後の読み取り) を、まず普通のプールで流して実行した文面を集め、次にその文面を
// 接続時に Prepare するプール (本番の EU/US と同じ AfterConnect) で流して、応答が同じで Prepare が 0 回になることを確かめる。
// ISUREAL_TEST_DSN は sql/tests/roundtrips.sh と同じ (001/002/007-image-location/004/005 を流した DB)。
func TestWarmedHandlersRespondTheSame(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set (see app/webapp/sql/tests/roundtrips.sh)")
	}
	open := func(tracer *recordingTracer, options ...stdlib.OptionOpenDB) *sqlx.DB {
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Tracer = tracer
		return sqlx.NewDb(stdlib.OpenDB(*cfg, options...), "pgx")
	}

	cold := &recordingTracer{}
	coldDB := open(cold)
	runRoundTripScenario(t, coldDB)
	coldDB.Close()
	if cold.prepares.Load() == 0 {
		t.Fatal("cold pool prepared nothing")
	}
	executed := cold.hot(1, 1000)

	setWarmStatements(executed)
	defer setWarmStatements(nil)
	warm := &recordingTracer{}
	warmDB := open(warm, stdlib.OptionAfterConnect(afterConnectWarm))
	defer warmDB.Close()
	runRoundTripScenario(t, warmDB)
	if got := currentWarmStatements(); len(got) != len(executed) {
		t.Fatalf("%d of %d statements were dropped while warming", len(executed)-len(got), len(executed))
	}
	if n := warm.prepares.Load(); n != 0 {
		t.Fatalf("warmed pool still prepared %d statement(s) on first use (cold pool: %d)", n, cold.prepares.Load())
	}
	t.Logf("cold pool: %d first-use prepares for %d statements; warmed pool: 0", cold.prepares.Load(), len(executed))
}
