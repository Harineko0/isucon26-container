package isureal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// TestSearchShortQueriesOnPostgres は、短い検索語を単純プロトコル (custom plan) で引いても、アプリの接続
// (plan_cache_mode = force_generic_plan、#153) のまま generic plan で引いても、本物の PostgreSQL 16 で
// 同じ結果 (総当たりで求めた期待値) を返し、1〜2 文字の語では custom plan の方が速いことを確かめる。
// ISUREAL_TEST_DSN が無ければ飛ばす (sql/tests/roundtrips.sh)。
func TestSearchShortQueriesOnPostgres(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set (see app/webapp/sql/tests/roundtrips.sh)")
	}
	connConfig, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	connConfig.RuntimeParams["plan_cache_mode"] = dbPlanCacheMode
	db := sqlx.NewDb(stdlib.OpenDB(*connConfig), "pgx")
	defer db.Close()
	db.SetMaxOpenConns(1) // 同じ接続で prepare した文 (generic plan) を使い回させる
	ctx := context.Background()

	key := strings.Repeat("cd", sha256.Size)
	sum := sha256.Sum256([]byte("search-token"))
	seed := []string{
		`TRUNCATE users, login_sessions, block_relations, user_search_norms, user_search_dirty CASCADE`,
		// 本番に近い 6,000 人。名前は 16 進の文字で、1 文字の語は半分ほどの利用者に一致する。
		`INSERT INTO users (id, username, secret_key_hash, display_name, region)
		 SELECT 'su' || i, 'u' || substr(md5(i::text), 1, 10), '` + key + `',
		        initcap(substr(md5((i * 7)::text), 1, 8)) || ' ' || substr(md5((i * 13)::text), 1, 6), 'europe'
		 FROM generate_series(1, 6000) i`,
		`INSERT INTO users (id, username, secret_key_hash, display_name, region) VALUES ('viewer', 'zz_viewer', '` + key + `', 'Viewer', 'europe')`,
		`INSERT INTO login_sessions (id, user_id, token_hash) VALUES ('sv', 'viewer', '` + hex.EncodeToString(sum[:]) + `')`,
		`INSERT INTO block_relations (id, blocker_id, blocked_id) VALUES ('b1', 'su1', 'viewer'), ('b2', 'viewer', 'su2')`,
		`ANALYZE`,
	}
	for _, statement := range seed {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	var users []User
	if err := db.SelectContext(ctx, &users, `SELECT `+strings.Join(userColumns, ", ")+` FROM users`); err != nil {
		t.Fatal(err)
	}
	if err := saveSearchNorms(ctx, db, users); err != nil {
		t.Fatal(err)
	}

	h := &HandlerContext{DB: db, Config: &Config{Region: "europe"}}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.GET("/api/users", searchUsersHandler(h), SessionMiddleware(h, true), RegionMiddleware(h))

	search := func(t *testing.T, q, beforeUsername, beforeID string) ([]string, bool) {
		t.Helper()
		target := "/api/users?q=" + url.QueryEscape(q)
		if beforeUsername != "" {
			target += "&beforeUsername=" + url.QueryEscape(beforeUsername) + "&beforeId=" + url.QueryEscape(beforeID)
		}
		rec := serveRoundTrip(e, "search-token", http.MethodGet, target, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s", target, rec.Code, rec.Body.String())
		}
		var body struct {
			Items []struct {
				ID       string `json:"id"`
				Username string `json:"username"`
			} `json:"items"`
			HasMore bool `json:"hasMore"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(body.Items))
		for i, item := range body.Items {
			ids[i] = item.ID
		}
		return ids, body.HasMore
	}
	// 総当たりの期待値: 自分とブロック関係の利用者を除き、正規化した名前に語を含む利用者を (username, id) 順に 50 件。
	sort.Slice(users, func(i, j int) bool {
		return users[i].Username < users[j].Username || users[i].Username == users[j].Username && users[i].ID < users[j].ID
	})
	expect := func(q, beforeUsername, beforeID string) ([]string, bool) {
		q = normalizeSearchText(q)
		var ids []string
		for _, u := range users {
			if u.ID == "viewer" || u.ID == "su1" || u.ID == "su2" {
				continue
			}
			if beforeUsername != "" && !(u.Username > beforeUsername || u.Username == beforeUsername && u.ID > beforeID) {
				continue
			}
			if strings.Contains(normalizeSearchText(u.Username), q) || strings.Contains(normalizeSearchText(u.DisplayName), q) {
				ids = append(ids, u.ID)
			}
		}
		if len(ids) > 50 {
			return ids[:50], true
		}
		return ids, false
	}
	serveRoundTrip(e, "search-token", http.MethodGet, "/api/users?q=a", "") // セッションを覚えさせる

	defer func(saved int) { searchCustomPlanMaxRunes = saved }(searchCustomPlanMaxRunes)
	for _, threshold := range []int{0, 3} { // 0: すべて generic plan (変更前)、3: 短い語だけ custom plan
		searchCustomPlanMaxRunes = threshold
		for _, tc := range []struct{ q, beforeUsername, beforeID string }{
			{"a", "", ""}, {"A", "", ""}, {"7", "", ""}, {"ab", "", ""},
			{"a", "u8", "su9999"}, {"ab", "u4", "su1"}, {"abc", "", ""}, {"u1a", "", ""}, {"zzzz", "", ""},
			{"'", "", ""}, {"%", "", ""}, {"_", "", ""}, {`\`, "", ""}, {"é", "", ""},
		} {
			got, gotMore := search(t, tc.q, tc.beforeUsername, tc.beforeID)
			want, wantMore := expect(tc.q, tc.beforeUsername, tc.beforeID)
			if !slices.Equal(got, want) || gotMore != wantMore {
				t.Errorf("threshold %d, q=%q before=(%q,%q): got %d items (hasMore %v), want %d (hasMore %v)\n got  %v\n want %v",
					threshold, tc.q, tc.beforeUsername, tc.beforeID, len(got), gotMore, len(want), wantMore, got, want)
			}
		}
	}

	// 1〜2 文字の語の速さ: generic plan (変更前) と custom plan (変更後) を交互に測り、中央値を比べる。
	median := func(d []time.Duration) time.Duration {
		slices.Sort(d)
		return d[len(d)/2]
	}
	for _, q := range []string{"a", "7", "ab"} {
		var generic, custom []time.Duration
		for range 15 {
			for _, threshold := range []int{0, 3} {
				searchCustomPlanMaxRunes = threshold
				start := time.Now()
				search(t, q, "", "")
				if threshold == 0 {
					generic = append(generic, time.Since(start))
				} else {
					custom = append(custom, time.Since(start))
				}
			}
		}
		g, c := median(generic), median(custom)
		t.Logf("q=%q: generic plan %v, custom plan %v (median of 15)", q, g, c)
		if c >= g {
			t.Errorf("q=%q: custom plan (%v) is not faster than the generic plan (%v)", q, c, g)
		}
	}
}
