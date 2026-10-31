package isureal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// EU/US は DB まで 1 往復 ~100ms (負荷中 0.2s)。pgx (QueryExecModeCacheStatement) は、ある文面を
// ある接続で初めて実行するとき、Parse/Describe の往復 (Prepare) をしてから実行するので 2 往復になる
// (pgx v5.11.0 conn.go getStatementDescription)。デプロイのたびにプロセスも接続も作り直されるので、
// ハンドラが 1 文でも、ベンチのたびに「よく使う文面 × プールの接続 40 本」ぶんの往復が余計にかかり、
// その間は接続を握るのでプール待ち (europe1 で 1 万回超) も伸ばしていた。
//
// そこで、よく実行した文面をファイルに覚えておき、次に起動したとき、接続を張った直後 (AfterConnect、
// warmUpDBPool が全接続を張るときも含む) に全部を 1 往復でまとめて Prepare し、pgx の文キャッシュに入れる。
// 中身は pgx が初回の実行で自分で作るものと同じ (同じ名前・同じ記述・同じ LRU) なので、実行の仕方・結果・
// エラー時の無効化は変わらない。asia は往復 ~1ms で得が無く、接続 80 本の記憶域だけ増えるので行わない。
//
// 接続時の Prepare は、そのデプロイの最初の initialize (sql/ の DDL を流す) より前に起こる。sql/ が前回と違うと、
// initialize で関数の戻り値や表の列が変わり、先に Prepare した文が初回の実行で "cached plan must not change
// result type" になりうる。そこでファイルには sql/ の指紋を添え、指紋が違えば使わずに空にする (そのデプロイは従来どおり)。

const (
	// preparedWarmMinCount 未満しか実行していない文面は覚えない。プール 40 本の接続あたり 5 回以上使う文面は、
	// 従来どおりでも走行中にほぼ全接続で Prepare されるので、先に Prepare しても DB のバックエンドが持つ
	// 文の記憶域 (手元の PG16 で、Prepare しただけのフィードの文が 1 つ約 260KB、画像の文が約 49KB) は増えない。
	// まれにしか使わない文面まで全接続に置くと、記憶域の少ない asia2 で増えるので入れない。
	preparedWarmMinCount = 200
	// preparedWarmLimit は覚える文面の上限 (pgx の文キャッシュは 512)。
	preparedWarmLimit = 60
	// preparedWarmMaxSQL より長い文面は数えない (動的に組んだ文で表が育たないように)。
	preparedWarmMaxSQL = 64 << 10
	// preparedWarmMaxTracked は数える文面の種類の上限。
	preparedWarmMaxTracked   = 4096
	preparedWarmSaveInterval = 30 * time.Second
	preparedWarmTimeout      = 5 * time.Second
)

var preparedWarmFile = envOrDefault("ISUREAL_PREPARED_STATEMENTS_FILE", "/home/isucon/isureal-prepared-statements.json")

// preparedWarmSchemaDir は initialize が流す SQL の置き場所 (initialize.go の ../sql/init.sh と同じ)。
var preparedWarmSchemaDir = "../sql"

// schemaFingerprint は dir の *.sql と init.sh の中身の指紋。
func schemaFingerprint(dir string) (string, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "", fmt.Errorf("no sql files in %s", dir)
	}
	files = append(files, filepath.Join(dir, "init.sh"))
	slices.Sort(files)
	h := sha256.New()
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %d\n", filepath.Base(file), len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type warmStatementsFile struct {
	Schema     string   `json:"schema"`
	Statements []string `json:"statements"`
}

// preparedWarmEnabled は文面の数え上げと接続時の Prepare をするリージョン。
func preparedWarmEnabled(region string) bool { return region != "asia" }

// statementCounter は文面ごとの実行回数 (pgx の QueryTracer として Query/Exec のたびに数える)。
type statementCounter struct {
	counts  sync.Map // string -> *atomic.Int64
	tracked atomic.Int64
}

var executedStatements = &statementCounter{}

func (s *statementCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	// 引数の無い Exec は pgx が単純プロトコルで送り、Prepare しないので数えない
	// (stdlib の QueryContext は結果の形式の指定を必ず 1 つ目の引数に付けるので、引数の無い Query は数える)。
	if len(data.Args) > 0 {
		s.record(data.SQL)
	}
	return ctx
}

func (*statementCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (s *statementCounter) record(sql string) {
	if sql == "" || len(sql) > preparedWarmMaxSQL {
		return
	}
	if v, ok := s.counts.Load(sql); ok {
		v.(*atomic.Int64).Add(1)
		return
	}
	if s.tracked.Load() >= preparedWarmMaxTracked {
		return
	}
	v, loaded := s.counts.LoadOrStore(sql, new(atomic.Int64))
	if !loaded {
		s.tracked.Add(1)
	}
	v.(*atomic.Int64).Add(1)
}

// hot は minCount 回以上実行した文面を、回数の多い順 (同数なら文面順) に limit 件まで返す。
func (s *statementCounter) hot(minCount int64, limit int) []string {
	type entry struct {
		sql string
		n   int64
	}
	var entries []entry
	s.counts.Range(func(key, value any) bool {
		if n := value.(*atomic.Int64).Load(); n >= minCount {
			entries = append(entries, entry{key.(string), n})
		}
		return true
	})
	slices.SortFunc(entries, func(a, b entry) int {
		if a.n != b.n {
			if a.n > b.n {
				return -1
			}
			return 1
		}
		if a.sql < b.sql {
			return -1
		}
		if a.sql > b.sql {
			return 1
		}
		return 0
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.sql
	}
	return out
}

// warmStatements は接続を張った直後に Prepare する文面 (起動時にファイルから読む)。
// 接続時に Prepare できなかった文面 (スキーマが変わったなど) は取り除く。
var warmStatements struct {
	mu   sync.Mutex
	list []string
}

func setWarmStatements(list []string) {
	warmStatements.mu.Lock()
	warmStatements.list = list
	warmStatements.mu.Unlock()
}

func currentWarmStatements() []string {
	warmStatements.mu.Lock()
	defer warmStatements.mu.Unlock()
	return warmStatements.list
}

func dropWarmStatements(bad []string) {
	if len(bad) == 0 {
		return
	}
	warmStatements.mu.Lock()
	defer warmStatements.mu.Unlock()
	warmStatements.list = slices.DeleteFunc(slices.Clone(warmStatements.list), func(sql string) bool {
		return slices.Contains(bad, sql)
	})
}

// loadWarmStatementsFile は path に覚えた文面を返す。sql/ の指紋 (schema) が違えば使わず、ファイルを空にする
// (別の sql/ のデプロイを挟んだあとで同じ sql/ に戻ったとき、間で変わったスキーマに対して Prepare しないように)。
func loadWarmStatementsFile(path, schema string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var file warmStatementsFile
	if err := json.Unmarshal(data, &file); err != nil {
		fmt.Fprintf(os.Stderr, "prepared-warm: ignoring %s: %v\n", path, err)
		return nil
	}
	if file.Schema != schema {
		fmt.Fprintf(os.Stderr, "prepared-warm: sql/ changed since %s was written; not preparing on connect\n", path)
		if err := saveWarmStatementsFile(path, schema, nil); err != nil {
			fmt.Fprintf(os.Stderr, "prepared-warm: failed to reset %s: %v\n", path, err)
		}
		return nil
	}
	list := file.Statements
	if len(list) > preparedWarmLimit {
		list = list[:preparedWarmLimit]
	}
	return list
}

func saveWarmStatementsFile(path, schema string, list []string) error {
	data, err := json.Marshal(warmStatementsFile{Schema: schema, Statements: list})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp-%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// saveHotStatementsLoop は、このプロセスでよく実行した文面を定期的にファイルへ書く (次の起動で使う)。
// よく実行した文面が 1 つも無いうち (起動直後や initialize だけのとき) は、前回のファイルを残す。
func saveHotStatementsLoop(path, schema string) {
	var saved []string
	for range time.Tick(preparedWarmSaveInterval) {
		hot := executedStatements.hot(preparedWarmMinCount, preparedWarmLimit)
		if len(hot) == 0 || slices.Equal(hot, saved) {
			continue
		}
		if err := saveWarmStatementsFile(path, schema, hot); err != nil {
			fmt.Fprintf(os.Stderr, "prepared-warm: failed to save %s: %v\n", path, err)
			continue
		}
		saved = hot
	}
}

// preparedWarmSentinelArgs は Prepare だけして実行はさせないための引数。アプリの文面の引数 ($n) は多くても
// 数十個で、拡張プロトコルの上限 (65535 個) を超える 65536 個と一致することはないので、どの文面でも数が合わず、
// pgx は実行を送る前 (引数の組み立て) で止まる。全文で同じスライスを共有する (1MiB を 1 回だけ確保)。
var preparedWarmSentinelArgs = make([]any, 65536)

// warmPreparedStatements は list の文面を conn で 1 往復でまとめて Prepare し、pgx の文キャッシュに入れる。
// pgx の SendBatch (QueryExecModeCacheStatement) は、キャッシュに無い文面をパイプラインで Prepare して
// キャッシュに入れ (1 往復)、続けて各文の引数を組み立てて実行を送る。引数の数が合わないので、最初の文の
// 組み立てで止まり、実行は 1 つも送られない (pgx v5.11.0 conn.go sendBatchExtendedWithDescription)。
// Prepare を拒まれた文面 (存在しない表など) は bad に入れて除き、残りでやり直す (最大 3 回)。
func warmPreparedStatements(ctx context.Context, conn *pgx.Conn, list []string) (warmed int, bad []string, err error) {
	for attempt := 0; attempt < 3 && len(list) > 0; attempt++ {
		batch := &pgx.Batch{}
		for _, sql := range list {
			batch.Queue(sql, preparedWarmSentinelArgs...)
		}
		err := conn.SendBatch(ctx, batch).Close()
		var preprocessing pgx.ErrPreprocessingBatch
		if !errors.As(err, &preprocessing) {
			if err == nil {
				// 起こらないはず (引数の数が合った = 実行された)。
				return 0, bad, errors.New("prepared-warm: batch executed unexpectedly")
			}
			return 0, bad, err
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			if conn.IsClosed() {
				// 通信の失敗やタイムアウト (pgconn は接続を閉じる)。
				return 0, bad, err
			}
			// 期待どおり: 全文の Prepare が済み、引数の組み立てで止まった。
			return len(list), bad, nil
		}
		// サーバが Prepare を拒んだ文面を除いてやり直す (pgx はこのバッチの文を全部キャッシュから外している)。
		failed := preprocessing.SQL()
		bad = append(bad, failed)
		list = slices.DeleteFunc(slices.Clone(list), func(sql string) bool { return sql == failed })
	}
	return 0, bad, nil
}

// afterConnectWarm は stdlib.OptionAfterConnect に渡す。失敗しても接続は使えるので、エラーは返さない
// (Prepare が済んでいない文面は、従来どおり初回の実行で pgx が Prepare する)。
func afterConnectWarm(_ context.Context, conn *pgx.Conn) error {
	list := currentWarmStatements()
	if len(list) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), preparedWarmTimeout)
	defer cancel()
	_, bad, err := warmPreparedStatements(ctx, conn, list)
	if len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "prepared-warm: dropped %d statement(s) that failed to prepare\n", len(bad))
		dropWarmStatements(bad)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "prepared-warm: %v\n", err)
	}
	return nil
}
