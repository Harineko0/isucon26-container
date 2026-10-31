package isureal

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type searchUsersRequest struct {
	Q              string `query:"q"`
	BeforeUsername string `query:"beforeUsername"`
	BeforeID       string `query:"beforeId"`
}

type searchUsersResponse struct {
	Items   []relationshipUserResponse `json:"items"`
	HasMore bool                       `json:"hasMore"`
}

func normalizeSearchText(value string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFC.String(value)))
}

func searchUsersHandler(h *HandlerContext) echo.HandlerFunc {
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
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		query := normalizeSearchText(input.Q)
		// 正規化済みの名前 (user_search_norms) で DB 側で照合し、51 件目の一致までだけを返す。
		// 正規化が古い・未作成の利用者 (stale) はアプリで照合する。
		// 候補は trigram 索引で引いた一致と、名前が変わって正規化が追いついていない利用者
		// (user_search_dirty、users のトリガーで記録) に限り、全 active 利用者は並べ替えない (#26)。
		// dirty のうち正規化が今の名前に追いついている (または消えた) 利用者は候補にしない。
		// 混ぜると候補が数百件になり、users と正規化の表を全件読むプランになる (#97)。
		var rows []struct {
			User
			Stale   bool `db:"stale"`
			Matched bool `db:"matched"`
		}
		err := h.DB.SelectContext(ctx, &rows, `
  WITH cand AS (
    SELECT user_id FROM user_search_norms WHERE username_norm LIKE $5 OR display_name_norm LIKE $5
    UNION
    SELECT d.user_id FROM user_search_dirty d
    JOIN users u ON u.id = d.user_id
    LEFT JOIN user_search_norms s ON s.user_id = d.user_id
    WHERE s.display_name IS DISTINCT FROM u.display_name
  )
  SELECT `+strings.Join(userColumns, ", ")+`, stale, matched FROM (
    SELECT u.*, m.stale, m.matched, count(*) FILTER (WHERE m.matched) OVER (ORDER BY u.username, u.id) AS n
    FROM cand JOIN users u ON u.id = cand.user_id
    LEFT JOIN user_search_norms s ON s.user_id = u.id
    CROSS JOIN LATERAL (SELECT s.display_name IS DISTINCT FROM u.display_name AS stale,
      COALESCE(s.display_name = u.display_name AND (strpos(s.username_norm, $4) > 0 OR strpos(s.display_name_norm, $4) > 0), false) AS matched) m
    WHERE u.status = 'active' AND u.id <> $1 AND ($2 = '' OR (u.username, u.id) > ($2, $3))
      AND NOT EXISTS (SELECT 1 FROM block_relations b
                      WHERE (b.blocker_id = $1 AND b.blocked_id = u.id) OR (b.blocked_id = $1 AND b.blocker_id = u.id))
  ) x
  WHERE (matched AND n <= 51) OR (stale AND n < 51)
  ORDER BY username, id
 `, viewer, input.BeforeUsername, input.BeforeID, query, "%"+escapeLike(query)+"%")
		if err != nil {
			return err
		}
		matchedUsers := make([]User, 0, 51)
		var staleUsers []User
		for _, row := range rows {
			user := row.User
			if row.Stale {
				staleUsers = append(staleUsers, user)
				if !strings.Contains(normalizeSearchText(user.Username), query) && !strings.Contains(normalizeSearchText(user.DisplayName), query) {
					continue
				}
			}
			matchedUsers = append(matchedUsers, user)
			if len(matchedUsers) == 51 {
				break
			}
		}
		if len(staleUsers) > 0 {
			saveSearchNormsOnce(h.DB, staleUsers)
		}
		hasMore := len(matchedUsers) > 50
		if hasMore {
			matchedUsers = matchedUsers[:50]
		}
		plan := newSignaturePlan()
		for i := range matchedUsers {
			if err := matchedUsers[i].setImageKey(); err != nil {
				return err
			}
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

type getUserRequest struct {
	UserID string `param:"userId"`
}

type getUserResponse relationshipUserResponse

func getUserHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getUserRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		// セッションの確認と同じ 1 文で引く (#82)。利用者が無ければ番兵の行 (found = false) が返る。
		var row struct {
			SessRegion *string `db:"sess_region"`
			Found      bool    `db:"found"`
			User
			Blocked  bool `db:"blocked"`
			IsFriend bool `db:"is_friend"`
		}
		err := getWithSession(c, h, &row, func() *string { return row.SessRegion }, `
   SELECT s.sess_region, r.id IS NOT NULL AS found, `+userColumnsCoalesced("r", "")+`,
     COALESCE(r.blocked, false) AS blocked, COALESCE(r.is_friend, false) AS is_friend
   FROM {session} LEFT JOIN LATERAL (
     SELECT `+strings.Join(userColumns, ", ")+`,
       EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=s.sess_user_id AND blocked_id=u.id) OR (blocker_id=u.id AND blocked_id=s.sess_user_id)) AS blocked,
       EXISTS(SELECT 1 FROM friendships WHERE user_low_id=least(s.sess_user_id, u.id) AND user_high_id=greatest(s.sess_user_id, u.id)) AS is_friend
     FROM users u WHERE u.id = $1 AND u.status = 'active'
   ) r ON true
  `, input.UserID)
		if err != nil {
			return err
		}
		if !row.Found || row.Blocked {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		user := row.User
		if err := user.setImageKey(); err != nil {
			return err
		}
		if user.AccountType != "certified_public" && !row.IsFriend {
			user.Bio = ""
		}
		return c.JSON(http.StatusOK, getUserResponse(newRelationshipUserResponse(user)))
	}
}

func newRelationshipUserResponse(user User) relationshipUserResponse {
	return relationshipUserResponse{ID: user.ID, Username: user.Username, DisplayName: user.DisplayName, ProfileImageURL: newGetMeResponse(user).ProfileImageURL, Bio: user.Bio, AccountType: user.AccountType}
}

// escapeLike は LIKE の特殊文字をエスケープする (既定のエスケープ文字は \)。
// strpos(s, q) > 0 と s LIKE '%' || escapeLike(q) || '%' は同じ結果になる。
func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s)
}

// saveSearchNorms は利用者名と表示名の検索用の正規化を保存する。表示名ごとに保存するので、
// 表示名が変わった利用者は検索時に stale として扱われ、アプリで照合したあと保存し直される。
func saveSearchNorms(ctx context.Context, db *sqlx.DB, users []User) error {
	ids := make([]string, len(users))
	usernameNorms := make([]string, len(users))
	displayNames := make([]string, len(users))
	displayNameNorms := make([]string, len(users))
	for i, user := range users {
		ids[i] = user.ID
		usernameNorms[i] = normalizeSearchText(user.Username)
		displayNames[i] = user.DisplayName
		displayNameNorms[i] = normalizeSearchText(user.DisplayName)
	}
	// 保存した表示名が今の表示名と同じ利用者だけ dirty から外す。保存の間に名前が変わっていれば
	// dirty に残り (トリガーが入れ直す)、次の検索でもアプリで照合される。
	_, err := db.ExecContext(ctx, `
  WITH saved AS (
    INSERT INTO user_search_norms (user_id, username_norm, display_name, display_name_norm)
    SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[])
    ON CONFLICT (user_id) DO UPDATE SET username_norm = EXCLUDED.username_norm,
      display_name = EXCLUDED.display_name, display_name_norm = EXCLUDED.display_name_norm
    RETURNING user_id, display_name
  )
  DELETE FROM user_search_dirty d USING saved, users u
  WHERE d.user_id = saved.user_id AND u.id = saved.user_id AND u.display_name = saved.display_name
 `, ids, usernameNorms, displayNames, displayNameNorms)
	return err
}

// cleanSearchDirty は、正規化が今の表示名に追いついた利用者と、消えた利用者を dirty から外す。
// 名前が変わった直後 (正規化が古い) の利用者は残すので、検索の正しさは変わらない。
func cleanSearchDirty(ctx context.Context, db *sqlx.DB) error {
	_, err := db.ExecContext(ctx, `
  DELETE FROM user_search_dirty d
  WHERE NOT EXISTS (
    SELECT 1 FROM users u LEFT JOIN user_search_norms s ON s.user_id = u.id
    WHERE u.id = d.user_id AND s.display_name IS DISTINCT FROM u.display_name)
 `)
	return err
}

// cleanSearchDirtyLoop は dirty を小さく保つ。登録や名前の変更のあと、正規化は非同期に保存されるので、
// 保存が済んだ利用者をここで外す。
func cleanSearchDirtyLoop(db *sqlx.DB) {
	for range time.Tick(2 * time.Second) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := cleanSearchDirty(ctx, db); err != nil {
			log.Printf("failed to clean user_search_dirty: %v", err)
		}
		cancel()
	}
}

// saveSearchNormsAsync は登録・表示名の変更のあとに正規化を保存する。応答は待たせない。
// 保存までの間はトリガーが入れた dirty の行があるので、検索はアプリで照合する。
func saveSearchNormsAsync(db *sqlx.DB, user User) {
	saveSearchNormsOnce(db, []User{user})
}

// searchNormsSaver は検索用の正規化の保存を 1 本の goroutine にまとめる。
// 以前は検索・登録・表示名の変更ごとに goroutine を立てて保存していたため、ベンチ中は保存 (1 回 13〜44ms) が
// 同時に何本も走り、その分だけ共有プールの接続を占めていた (#68: プール 80/80 のまま 1 ベンチで
// 25,000 回・1.5 時間の接続待ち)。ここでは保存待ちの利用者を溜め、1 本の goroutine が溜まった分を
// まとめて 1 文で保存する。保存に使う接続は常に高々 1 本で、文の数も減る (DB の CPU も減る)。
// 正しさは変わらない: 保存が済むまでの間は dirty / stale の利用者として検索がアプリで照合する。
// 保存の間に名前が変わっていれば dirty に残る (saveSearchNorms)。
type searchNormsSaver struct {
	save func(ctx context.Context, users []User) error

	mu       sync.Mutex
	pending  map[string]User   // 保存待ち (同じ利用者は新しい内容で上書き)
	inflight map[string]string // 保存中の利用者 id → 表示名
	wake     chan struct{}
}

// searchNormsBatchLimit は 1 文で保存する利用者数の上限。
const searchNormsBatchLimit = 500

func newSearchNormsSaver(save func(ctx context.Context, users []User) error) *searchNormsSaver {
	s := &searchNormsSaver{
		save:     save,
		pending:  map[string]User{},
		inflight: map[string]string{},
		wake:     make(chan struct{}, 1),
	}
	go s.run()
	return s
}

// add は users を保存待ちに入れる。同じ表示名で保存中の利用者は積まない (同時に走る検索が同じ利用者の
// 保存を何度も積んでいた: 1 ベンチで登録 560 件に対し保存 1318 回)。
func (s *searchNormsSaver) add(users []User) {
	added := false
	s.mu.Lock()
	for _, user := range users {
		if _, ok := s.pending[user.ID]; !ok {
			if name, saving := s.inflight[user.ID]; saving && name == user.DisplayName {
				continue
			}
		}
		s.pending[user.ID] = user
		added = true
	}
	s.mu.Unlock()
	if !added {
		return
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// take は保存待ちを id 順に最大 searchNormsBatchLimit 件取り出し、保存中にする。
// id 順にそろえるのは、3 台が同時に保存したときに行ロックを同じ順で取るため。
func (s *searchNormsSaver) take() []User {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil
	}
	ids := make([]string, 0, len(s.pending))
	for id := range s.pending {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > searchNormsBatchLimit {
		ids = ids[:searchNormsBatchLimit]
	}
	batch := make([]User, len(ids))
	for i, id := range ids {
		batch[i] = s.pending[id]
		delete(s.pending, id)
		s.inflight[id] = batch[i].DisplayName
	}
	return batch
}

func (s *searchNormsSaver) done(batch []User) {
	s.mu.Lock()
	for _, user := range batch {
		delete(s.inflight, user.ID)
	}
	s.mu.Unlock()
}

func (s *searchNormsSaver) run() {
	for range s.wake {
		for {
			batch := s.take()
			if len(batch) == 0 {
				break
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.save(ctx, batch); err != nil {
				log.Printf("failed to save search norms: %v", err)
			}
			cancel()
			s.done(batch)
		}
	}
}

var (
	searchNorms     *searchNormsSaver
	searchNormsOnce sync.Once
)

// saveSearchNormsOnce は users の正規化を保存待ちに入れる。保存は searchNormsSaver の 1 本の goroutine が行う。
func saveSearchNormsOnce(db *sqlx.DB, users []User) {
	if len(users) == 0 {
		return
	}
	searchNormsOnce.Do(func() {
		searchNorms = newSearchNormsSaver(func(ctx context.Context, users []User) error {
			return saveSearchNorms(ctx, db, users)
		})
	})
	searchNorms.add(users)
}

// refreshSearchNorms は正規化が無い・古い利用者をまとめて保存し直す (起動時と初期化後)。
func refreshSearchNorms(db *sqlx.DB) {
	ctx := context.Background()
	// 002-indexes.sql と同じ定義。initialize の前にデプロイされても検索が動くよう起動時にも作る。
	// 3 台が同時に起動すると DDL が競合して失敗することがあるが、どれか 1 台が作れば足りる。
	for _, stmt := range searchNormsSchema {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			log.Printf("search norms schema: %v", err)
		}
	}
	if err := cleanSearchDirty(ctx, db); err != nil {
		log.Printf("failed to clean user_search_dirty: %v", err)
	}
	var users []User
	if err := db.SelectContext(ctx, &users, `
  SELECT u.id, u.username, u.display_name FROM users u
  LEFT JOIN user_search_norms s ON s.user_id = u.id
  WHERE s.display_name IS DISTINCT FROM u.display_name
  UNION
  SELECT u.id, u.username, u.display_name FROM user_search_dirty d JOIN users u ON u.id = d.user_id
 `); err != nil {
		log.Printf("failed to load users for search norms: %v", err)
		return
	}
	if len(users) == 0 {
		return
	}
	if err := saveSearchNorms(ctx, db, users); err != nil {
		log.Printf("failed to save search norms: %v", err)
	}
}

// searchNormsSchema は sql/002-indexes.sql の利用者検索の部分と同じ (変えるときは両方)。
var searchNormsSchema = []string{
	`CREATE TABLE IF NOT EXISTS user_search_norms (
  user_id text PRIMARY KEY, username_norm text NOT NULL, display_name text NOT NULL, display_name_norm text NOT NULL)`,
	`CREATE EXTENSION IF NOT EXISTS pg_trgm`,
	`CREATE INDEX IF NOT EXISTS user_search_norms_username_trgm ON user_search_norms USING gin (username_norm gin_trgm_ops)`,
	`CREATE INDEX IF NOT EXISTS user_search_norms_display_name_trgm ON user_search_norms USING gin (display_name_norm gin_trgm_ops)`,
	`CREATE TABLE IF NOT EXISTS user_search_dirty (user_id text PRIMARY KEY)`,
	`CREATE OR REPLACE FUNCTION isureal_mark_search_dirty() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO user_search_dirty (user_id) VALUES (NEW.id) ON CONFLICT DO NOTHING;
  RETURN NULL;
END $$`,
	`CREATE OR REPLACE TRIGGER users_search_dirty_insert AFTER INSERT ON users
  FOR EACH ROW EXECUTE FUNCTION isureal_mark_search_dirty()`,
	`CREATE OR REPLACE TRIGGER users_search_dirty_update AFTER UPDATE OF display_name ON users
  FOR EACH ROW WHEN (OLD.display_name IS DISTINCT FROM NEW.display_name) EXECUTE FUNCTION isureal_mark_search_dirty()`,
	`INSERT INTO user_search_dirty (user_id)
  SELECT u.id FROM users u LEFT JOIN user_search_norms s ON s.user_id = u.id
  WHERE s.display_name IS DISTINCT FROM u.display_name
  ON CONFLICT DO NOTHING`,
}
