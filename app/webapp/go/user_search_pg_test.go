package isureal

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// 利用者検索 (GET /api/users) の SQL を、002-indexes.sql (トリガー込み) を当てた PostgreSQL で確かめる。
// 名前を変えた直後・登録直後 (正規化が古い・無い) の利用者が新しい名前で出ること、ブロックと
// ページングが効くこと。ISUREAL_TEST_DSN が無ければ飛ばす。
func TestSearchUsersOnPostgres(t *testing.T) {
	dsn := os.Getenv("ISUREAL_TEST_DSN")
	if dsn == "" {
		t.Skip("ISUREAL_TEST_DSN is not set")
	}
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db := sqlx.NewDb(stdlib.OpenDB(*cfg), "pgx")
	defer db.Close()
	h := &HandlerContext{DB: db, Config: &Config{Region: "asia"}}
	e := echo.New()
	e.HTTPErrorHandler = APIErrorHandler
	e.GET("/api/users", searchUsersHandler(h), func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			c.Set("session", &Session{UserID: "viewer", Region: "asia"})
			return next(c)
		}
	})
	for _, q := range []string{
		`TRUNCATE users, user_search_norms, user_search_dirty, block_relations CASCADE`,
		`INSERT INTO users(id,username,secret_key_hash,display_name,region) VALUES
		   ('viewer','viewer',repeat('ab',32),'Viewer','asia'),
		   ('u-alice','alice',repeat('ab',32),'Alice','asia'),
		   ('u-bob','user_b',repeat('ab',32),'Bob','asia'),
		   ('u-blocked','blocked_x',repeat('ab',32),'Zed','asia')`,
		`INSERT INTO users(id,username,secret_key_hash,display_name,region)
		   SELECT 'p-'||lpad(i::text,2,'0'),'page'||lpad(i::text,2,'0'),repeat('ab',32),'Pager','asia' FROM generate_series(1,60) i`,
		`INSERT INTO user_search_norms(user_id,username_norm,display_name,display_name_norm)
		   SELECT id,username,display_name,lower(display_name) FROM users`,
		`DELETE FROM user_search_dirty`,
		`INSERT INTO block_relations(id,blocker_id,blocked_id) VALUES ('b1','viewer','u-blocked')`,
		// 名前の変更と登録 (正規化はまだ古い・無い。トリガーが dirty に入れる)
		`UPDATE users SET display_name='Carol' WHERE id='u-bob'`,
		`INSERT INTO users(id,username,secret_key_hash,display_name,region) VALUES ('u-new','newbie',repeat('ab',32),'Dave','asia')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("seed %s: %+v", q, err)
		}
	}
	search := func(params url.Values) ([]string, bool) {
		t.Helper()
		rec := serveRoundTrip(e, "", http.MethodGet, "/api/users?"+params.Encode(), "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%v: %d %s", params, rec.Code, rec.Body.String())
		}
		var page struct {
			Items []struct {
				ID       string `json:"id"`
				Username string `json:"username"`
			} `json:"items"`
			HasMore bool `json:"hasMore"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(page.Items))
		for i, item := range page.Items {
			ids[i] = item.ID
		}
		return ids, page.HasMore
	}
	for _, tc := range []struct {
		q    string
		want string
	}{
		{"carol", "[u-bob]"}, // 改名直後の新しい表示名で出る
		{"bob", "[]"},        // 古い表示名では出ない
		{"dave", "[u-new]"},  // 登録直後 (正規化が無い)
		{"NEWB", "[u-new]"},  // 大文字小文字を区別しない
		{"zed", "[]"},        // ブロックした相手は出ない
		{"viewer", "[]"},     // 自分は出ない
		{"ALI", "[u-alice]"},
		{"_", "[u-bob]"}, // LIKE の _ は文字どおり (blocked_x はブロック済み)
	} {
		ids, more := search(url.Values{"q": {tc.q}})
		if got := fmt.Sprint(ids); got != tc.want || more {
			t.Errorf("q=%s: got %s hasMore=%v; want %s", tc.q, got, more, tc.want)
		}
	}
	ids, more := search(url.Values{"q": {"page"}})
	if len(ids) != 50 || !more || ids[0] != "p-01" || ids[49] != "p-50" {
		t.Fatalf("page 1: %d items hasMore=%v first=%v", len(ids), more, ids)
	}
	ids, more = search(url.Values{"q": {"page"}, "beforeUsername": {"page50"}, "beforeId": {"p-50"}})
	if len(ids) != 10 || more || ids[0] != "p-51" || ids[9] != "p-60" {
		t.Fatalf("page 2: %d items hasMore=%v %v", len(ids), more, ids)
	}
}
