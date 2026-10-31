package isureal

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

// usersCache は users 表のアプリ内レプリカ (全リージョンの各プロセスが 1 つずつ持つ)。
//
// 整合性は DB の版番号 (cache_versions.version、009-users-cache.sql) で守る。users への書き込みは
// 同じトランザクションのトリガーで版を 1 進め、変わった利用者の id を users_changes に残す。
// 読むときは 1 文で「今の版」「覚えている版より後の変更 (利用者の今の行つき)」「閲覧者のブロック関係」を
// 同じスナップショットで取り、メモリの表を DB と同じ状態にしてから検索する。版の更新は行ロック越しなので
// 版の順序 = コミットの順序であり、版が同じなら未反映の変更は無い (リージョン間でも TTL も非同期も無い)。
//
// 失敗時: 同期の SQL に失敗したら、呼び出し元は従来の DB 検索にそのまま戻る (古い値を返す経路は無い)。
// initialize: reset.sql がトリガーを止めて users を書き換えるので、版を進めて user_id = '*' の行を残す。
// どのリージョンも次の検索で '*' を見て全件を読み直す。再起動時は起動時と最初の検索で全件を読む。
type usersCache struct {
	mu      sync.RWMutex
	version int64 // -1: 未読込
	users   map[string]*cachedUser
	sorted  []*cachedUser // (username, id) 昇順。C.UTF-8 の照合なのでバイト順 = Go の文字列比較。
}

type cachedUser struct {
	User
	usernameNorm    string
	displayNameNorm string
}

var theUsersCache = &usersCache{version: -1, users: map[string]*cachedUser{}}

func newCachedUser(user User) (*cachedUser, error) {
	if err := user.setImageKey(); err != nil {
		return nil, err
	}
	return &cachedUser{User: user, usernameNorm: normalizeSearchText(user.Username), displayNameNorm: normalizeSearchText(user.DisplayName)}, nil
}

func cachedUserLess(a, b *cachedUser) bool {
	if a.Username != b.Username {
		return a.Username < b.Username
	}
	return a.ID < b.ID
}

// loadAll は users 全件と版を同じスナップショットで読み、表を置き換える。
func (c *usersCache) loadAll(ctx context.Context, db sqlx.QueryerContext) error {
	var rows []struct {
		Version int64 `db:"version"`
		User
	}
	if err := sqlx.SelectContext(ctx, db, &rows, `
  SELECT (SELECT version FROM cache_versions WHERE name = 'users') AS version, `+strings.Join(userColumns, ", ")+`
  FROM users`); err != nil {
		return err
	}
	users := make(map[string]*cachedUser, len(rows))
	sorted := make([]*cachedUser, 0, len(rows))
	version := int64(0)
	for _, row := range rows {
		version = row.Version
		cu, err := newCachedUser(row.User)
		if err != nil {
			return err
		}
		users[cu.ID] = cu
		sorted = append(sorted, cu)
	}
	sort.Slice(sorted, func(i, j int) bool { return cachedUserLess(sorted[i], sorted[j]) })
	c.mu.Lock()
	c.users = users
	c.sorted = sorted
	c.version = version
	c.mu.Unlock()
	return nil
}

func loadUsersCache(db *sqlx.DB, reason string) {
	if err := theUsersCache.loadAll(context.Background(), db); err != nil {
		fmt.Fprintf(os.Stderr, "users cache load (%s) failed: %v\n", reason, err)
		return
	}
	theUsersCache.mu.RLock()
	fmt.Fprintf(os.Stderr, "users cache load (%s): %d users, version %d\n", reason, len(theUsersCache.users), theUsersCache.version)
	theUsersCache.mu.RUnlock()
}

// removeSorted / insertSorted は sorted を (username, id) 順に保つ。
func (c *usersCache) removeSorted(cu *cachedUser) {
	i := sort.Search(len(c.sorted), func(i int) bool { return !cachedUserLess(c.sorted[i], cu) })
	for j := i; j < len(c.sorted) && !cachedUserLess(cu, c.sorted[j]); j++ {
		if c.sorted[j] == cu {
			c.sorted = append(c.sorted[:j], c.sorted[j+1:]...)
			return
		}
	}
}

func (c *usersCache) insertSorted(cu *cachedUser) {
	i := sort.Search(len(c.sorted), func(i int) bool { return !cachedUserLess(c.sorted[i], cu) })
	c.sorted = append(c.sorted, nil)
	copy(c.sorted[i+1:], c.sorted[i:])
	c.sorted[i] = cu
}

type usersSyncRow struct {
	Version   int64   `db:"version"`
	Blocked   *string `db:"blocked"`
	ChangedID *string `db:"changed_id"`
	Present   bool    `db:"present"`
	User
}

var usersSyncQuery = `
  SELECT (SELECT version FROM cache_versions WHERE name = 'users') AS version,
    (SELECT string_agg(CASE WHEN blocker_id = $1 THEN blocked_id ELSE blocker_id END, ',')
     FROM block_relations WHERE blocker_id = $1 OR blocked_id = $1) AS blocked,
    c.user_id AS changed_id, u.id IS NOT NULL AS present, ` + userColumnsCoalesced("u", "") + `
  FROM (SELECT 1) one
  LEFT JOIN users_changes c ON c.version > $2
  LEFT JOIN users u ON u.id = c.user_id`

// sync はメモリの表を DB の今の状態にそろえ、閲覧者のブロック関係を返す。往復は普段 1 回 (全件読み直しのときだけ 2 回)。
func (c *usersCache) sync(ctx context.Context, db *sqlx.DB, viewer string) (blocked map[string]bool, err error) {
	c.mu.RLock()
	last := c.version
	c.mu.RUnlock()
	var rows []usersSyncRow
	if err := sqlx.SelectContext(ctx, db, &rows, usersSyncQuery, viewer, last); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("users cache sync returned no rows")
	}
	blocked = map[string]bool{}
	for _, id := range splitIDs(rows[0].Blocked) {
		blocked[id] = true
	}
	dbVersion := rows[0].Version
	full := last < 0 || dbVersion < last
	for _, row := range rows {
		if row.ChangedID != nil && *row.ChangedID == "*" {
			full = true
		}
	}
	if full {
		// 全件読み直し。ブロック関係は上のスナップショットのもの (users の状態はそれ以降) でよい:
		// どちらも要求の開始より後の状態で、要求前に確定した書き込みは必ず含まれる。
		return blocked, c.loadAll(ctx, db)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if dbVersion <= c.version {
		// 並行する要求が同じかそれより新しい状態を反映済み (その変更集合はこの集合を含む)。
		return blocked, nil
	}
	for _, row := range rows {
		if row.ChangedID == nil {
			continue
		}
		id := *row.ChangedID
		if old, ok := c.users[id]; ok {
			c.removeSorted(old)
			delete(c.users, id)
		}
		if !row.Present {
			continue
		}
		cu, err := newCachedUser(row.User)
		if err != nil {
			return nil, err
		}
		c.users[id] = cu
		c.insertSorted(cu)
	}
	c.version = dbVersion
	return blocked, nil
}

// search は searchUsersHandler の SQL と同じ条件・並びで、メモリから最大 limit 件を返す。
func (c *usersCache) search(viewer, query, beforeUsername, beforeID string, blocked map[string]bool, limit int) []User {
	c.mu.RLock()
	defer c.mu.RUnlock()
	start := 0
	if beforeUsername != "" {
		key := &cachedUser{User: User{Username: beforeUsername, ID: beforeID}}
		start = sort.Search(len(c.sorted), func(i int) bool { return cachedUserLess(key, c.sorted[i]) })
	}
	out := make([]User, 0, limit)
	for _, cu := range c.sorted[start:] {
		if cu.Status != "active" || cu.ID == viewer || blocked[cu.ID] {
			continue
		}
		if !strings.Contains(cu.usernameNorm, query) && !strings.Contains(cu.displayNameNorm, query) {
			continue
		}
		out = append(out, cu.User)
		if len(out) == limit {
			break
		}
	}
	return out
}

// searchUsersMemHandler は GET /api/users をメモリの利用者表で処理する。同期に失敗したら fallback (従来の DB 検索)。
func searchUsersMemHandler(h *HandlerContext, fallback echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input searchUsersRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if !validUserText(input.Q, 1, 100) {
			return NewAPIError(http.StatusBadRequest, "invalid_relationship")
		}
		if (input.BeforeUsername == "") != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		viewer := GetSession(c).UserID
		query := normalizeSearchText(input.Q)
		blocked, err := theUsersCache.sync(c.Request().Context(), h.DB, viewer)
		if err != nil {
			fmt.Fprintf(os.Stderr, "users cache sync failed, falling back to DB search: %v\n", err)
			return fallback(c)
		}
		matchedUsers := theUsersCache.search(viewer, query, input.BeforeUsername, input.BeforeID, blocked, 51)
		hasMore := len(matchedUsers) > 50
		if hasMore {
			matchedUsers = matchedUsers[:50]
		}
		plan := newSignaturePlan()
		for i := range matchedUsers {
			plan.addProfile(matchedUsers[i])
		}
		plan.run()
		items := make([]relationshipUserResponse, 0, len(matchedUsers))
		for _, user := range matchedUsers {
			if user.AccountType != "certified_public" {
				user.Bio = ""
			}
			items = append(items, newRelationshipUserResponse(user))
		}
		return c.JSON(http.StatusOK, searchUsersResponse{Items: items, HasMore: hasMore})
	}
}
