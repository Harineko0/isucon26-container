package isureal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v5"
)

type Session struct {
	Region    string    `db:"region" json:"-"`
	Token     string    `db:"-" json:"-"`
	UserID    string    `db:"user_id"`
	TokenHash string    `db:"token_hash"`
	CreatedAt time.Time `db:"created_at"`
}

func GetSession(c *echo.Context) *Session {
	sess, ok := c.Get("session").(*Session)
	if !ok {
		return nil
	}
	return sess
}

func sessionTokenHash(c *echo.Context) (string, bool, error) {
	authHeader := c.Request().Header.Get(echo.HeaderAuthorization)
	if authHeader == "" {
		return "", false, nil
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		c.Logger().Warn("Invalid authorization header format(not started with Bearer)")
		return "", false, NewAPIError(http.StatusUnauthorized, "invalid_session")
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")
	tokenHashBytes := sha256.Sum256([]byte(token))
	return hex.EncodeToString(tokenHashBytes[:]), true, nil
}

func lookupSession(ctx context.Context, db *sqlx.DB, tokenHash string) (*Session, error) {
	var session Session
	err := db.GetContext(ctx, &session, `
		SELECT s.user_id, s.token_hash, s.created_at, u.region
		FROM login_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.active = true AND u.status = 'active'
	`, tokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	knownSessions.Store(tokenHash, session)
	return &session, nil
}

// トークンと利用者の対応は変わらないので、一度確認したセッションを覚えておき、
// 読み取りの GET ではセッションの確認と処理を並行に進める。応答はいったんバッファに溜め、
// 確認結果が覚えていた内容と一致したときだけ返す。一致しなければ通常どおりやり直す。
// 1 件 200 バイト程度。上限を超えて捨てても、次の確認で覚え直すだけ。
var knownSessions = &boundedMap{limit: 100_000}

type bufferedResponseWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *bufferedResponseWriter) Header() http.Header { return w.header }

func (w *bufferedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *bufferedResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(b)
}

// deferredSessionRoutes の GET はハンドラの 1 文でセッションを確認するので、投機的には処理しない。
// asia は DB との往復が 1ms なので、並行にしても 1ms しか縮まないのに GET 1 回で接続を 2 本使い、
// プール待ちの最大要因になっていた (#68)。往復 100ms の EU/US だけで使う。
func speculativeSessionAllowed(c *echo.Context, region string) bool {
	return region != "asia" && c.Request().Method == http.MethodGet && !deferredSessionRoutes[c.Request().Method+" "+c.RouteInfo().Path]
}

func SessionMiddleware(handlerContext *HandlerContext, authRequired bool) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			tokenHash, ok, err := sessionTokenHash(c)
			if err != nil {
				return err
			}
			ctx := c.Request().Context()
			if ok && speculativeSessionAllowed(c, handlerContext.Config.Region) {
				if cached, found := knownSessions.Load(tokenHash); found {
					known := cached.(Session)
					type result struct {
						session *Session
						err     error
					}
					validated := make(chan result, 1)
					go func() {
						session, err := lookupSession(ctx, handlerContext.DB, tokenHash)
						validated <- result{session, err}
					}()
					original := c.Response()
					buffer := &bufferedResponseWriter{header: http.Header{}}
					c.SetResponse(echo.NewResponse(buffer, c.Logger()))
					speculative := known
					c.Set("session", &speculative)
					handlerErr := next(c)
					checked := <-validated
					c.SetResponse(original)
					if checked.err != nil {
						return checked.err
					}
					if checked.session != nil && checked.session.UserID == known.UserID && checked.session.Region == known.Region {
						c.Set("session", checked.session)
						if buffer.status != 0 {
							for key, values := range buffer.header {
								original.Header()[key] = values
							}
							original.WriteHeader(buffer.status)
							if _, err := original.Write(buffer.body.Bytes()); err != nil {
								return err
							}
						}
						return handlerErr
					}
					// 覚えていた内容と違ったので、確認済みのセッションで処理し直す(GET は読み取りのみ)。
					c.Set("session", checked.session)
					if checked.session == nil && authRequired {
						return NewAPIError(http.StatusUnauthorized, "invalid_session")
					}
					return next(c)
				}
			}
			if ok && deferredSessionRoutes[c.Request().Method+" "+c.RouteInfo().Path] {
				if cached, found := knownSessions.Load(tokenHash); found {
					known := cached.(Session)
					c.Set("session", &known)
					c.Set("deferred_session", tokenHash)
					err := next(c)
					if err == nil || c.Get(deferredSessionCheckedKey) != nil {
						return err
					}
					if response, _ := echo.UnwrapResponse(c.Response()); response != nil && response.Committed {
						return err
					}
					// ハンドラがセッションを確認する SQL に達する前に失敗した (入力検証の 400 など)。
					// 参照実装は SessionMiddleware → RegionMiddleware → ハンドラの順なので、401 → 403 を
					// ハンドラのエラーより優先する。成功経路では通らないので往復は増えない。
					return verifyDeferredSession(c, handlerContext, tokenHash, err)
				}
			}
			var session *Session
			if ok {
				if session, err = lookupSession(ctx, handlerContext.DB, tokenHash); err != nil {
					return err
				}
			}
			if session == nil && authRequired {
				return NewAPIError(http.StatusUnauthorized, "invalid_session")
			}
			c.Set("session", session)
			return next(c)
		}
	}
}

func InvalidateLoginSession(db *sqlx.DB, userID string, tokenHash string) error {
	knownSessions.Delete(tokenHash)
	var lockedUserID string
	if err := db.Get(&lockedUserID, `
		WITH locked AS (SELECT id FROM users WHERE id = $1 FOR NO KEY UPDATE),
		invalidated AS (
			UPDATE login_sessions
			SET active = false, invalidated_at = clock_timestamp(), reason = 'logout'
			WHERE user_id = $1 AND active = true AND token_hash = $2 AND EXISTS (SELECT 1 FROM locked)
		)
		SELECT id FROM locked
	`, userID, tokenHash); err != nil {
		return fmt.Errorf("failed to lock user: %w", err)
	}
	return nil
}

// 2026-09-03: リファクタリング後にこの関数の利用がないようです。消すのはいかがでしょうか
func CreateLoginSession(db *sqlx.DB, userID string) (*Session, error) {
	ctx := context.Background()
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback()
	session, err := createLoginSessionTx(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit transaction: %w", err)
	}
	return session, nil
}

func createLoginSessionTx(ctx context.Context, tx *sqlx.Tx, userID string) (*Session, error) {
	randomToken := uuid.NewString()
	tokenHashBytes := sha256.Sum256([]byte(randomToken))
	tokenHash := hex.EncodeToString(tokenHashBytes[:])
	var lockedUserID string
	if err := tx.GetContext(ctx, &lockedUserID, `SELECT id FROM users WHERE id = $1 AND status = 'active' FOR NO KEY UPDATE`, userID); err != nil {
		return nil, fmt.Errorf("failed to lock user: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
  UPDATE login_sessions
  SET active = false, invalidated_at = clock_timestamp(), reason = 'replaced'
  WHERE user_id = $1 AND active = true
 `, userID); err != nil {
		return nil, fmt.Errorf("failed to invalidate sessions: %w", err)
	}
	sessionID := uuid.NewString()
	if _, err := tx.ExecContext(ctx, `
  INSERT INTO login_sessions (id, user_id, token_hash)
  VALUES ($1, $2, $3)
 `, sessionID, userID, tokenHash); err != nil {
		return nil, fmt.Errorf("failed to insert session: %w", err)
	}
	session, err := getLoginSession(ctx, tx, sessionID)
	if err != nil {
		return nil, err
	}
	session.Token = randomToken
	return session, nil
}

func getLoginSession(ctx context.Context, db sqlx.QueryerContext, sessionID string) (*Session, error) {
	var session Session
	if err := sqlx.GetContext(ctx, db, &session, `SELECT user_id, token_hash, created_at FROM login_sessions WHERE id = $1`, sessionID); err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	return &session, nil
}

// 書き込みでは投機的に処理できないので、覚えているセッションがあれば確認を書き込みの SQL に
// 同居させる (1 往復減らす)。対象のルートは deferredSessionRoutes に限る。
var deferredSessionRoutes = map[string]bool{
	"POST /api/posts":                   true,
	"POST /api/posts/:postId/comments":  true,
	"POST /api/posts/:postId/realmojis": true,
	"POST /api/friend-requests":         true,
	"PUT /api/follows/:userId":          true,
	"DELETE /api/follows/:userId":       true,
	"PUT /api/blocks/:userId":           true,
	"DELETE /api/blocks/:userId":        true,
	"DELETE /api/friendships/:userId":   true,
}

// 読み取りだが、セッションの確認をハンドラの SQL に同居させるルート (notification_api.go, posting_window_api.go)。
// 下の 5 つは getWithSession / selectWithSession で 1 文にする (#82)。EU/US では投機的な確認
// (GET 1 回で接続 2〜3 本) をやめ、asia でも lookupSession の 1 往復 (接続の取り出し 1 回) を省く。
func init() {
	deferredSessionRoutes["GET /api/notifications"] = true
	deferredSessionRoutes["GET /api/posting-window/current"] = true
	deferredSessionRoutes["GET /api/me"] = true
	deferredSessionRoutes["GET /api/users/:userId"] = true
	deferredSessionRoutes["GET /api/relationships/:kind"] = true
	deferredSessionRoutes["GET /api/posts/:postId/comments"] = true
	deferredSessionRoutes["GET /api/posts/:postId/realmojis"] = true
	deferredSessionRoutes["GET /api/memories"] = true
	deferredSessionRoutes["GET /api/memories/:date"] = true
	deferredSessionRoutes["GET /api/me/export"] = true
	deferredSessionRoutes["GET /api/users"] = true
}

// deferredSessionCheckedKey は、deferred 経路でハンドラがセッションを確認する SQL を実行したことを示す。
// これが無いままハンドラがエラーを返したら、SessionMiddleware がセッションを確認してから返す。
const deferredSessionCheckedKey = "deferred_session_checked"

// lookupSessionFunc はテストで差し替えるためのもの。
var lookupSessionFunc = lookupSession

// verifyDeferredSession は、deferred 経路のハンドラがセッションを確認する前に handlerErr を返したときに
// 呼ぶ。セッションが無効なら 401、リージョン違いなら 403、問題なければ handlerErr をそのまま返す。
func verifyDeferredSession(c *echo.Context, h *HandlerContext, tokenHash string, handlerErr error) error {
	session, err := lookupSessionFunc(c.Request().Context(), h.DB, tokenHash)
	if err != nil {
		return err
	}
	region := ""
	if session != nil {
		region = session.Region
		if anyRegionDeferredRoutes[c.Request().Method+" "+c.RouteInfo().Path] {
			// RegionMiddleware の無いルートなので、リージョンが違っても 403 にしない。
			region = h.Config.Region
		}
	}
	if err := rejectDeferredSession(c, h, tokenHash, session != nil, region); err != nil {
		return err
	}
	return handlerErr
}

// rejectDeferredSession は、ハンドラの SQL でセッションを確認した結果が 401/403 のときの応答を返す。
// found が false ならセッションが無効 (401)、region がこのサーバのリージョンと違えば 403。問題なければ nil。
func rejectDeferredSession(c *echo.Context, h *HandlerContext, tokenHash string, found bool, region string) error {
	c.Set(deferredSessionCheckedKey, true)
	if !found {
		knownSessions.Delete(tokenHash)
		c.Set("session", nil)
		return NewAPIError(http.StatusUnauthorized, "invalid_session")
	}
	if region != h.Config.Region {
		knownSessions.Delete(tokenHash)
		return NewAPIError(http.StatusForbidden, "region_mismatch")
	}
	return nil
}

func deferredSession(c *echo.Context) (string, bool) {
	tokenHash, ok := c.Get("deferred_session").(string)
	return tokenHash, ok
}

// callWithSession は `call` (例: `isureal_relationship($1, {user}, $2, $3)`) を、
// セッションの確認と同じ 1 文で実行する。{user} はセッションの利用者 id に置き換わる。
// 結果の行は sess_region 列を持つ構造体で受ける。セッションが無効なら 401、リージョン違いなら 403。
func callWithSession(c *echo.Context, h *HandlerContext, dest any, sessRegion func() *string, call string, args ...any) error {
	return callWithSessionColumns(c, h, dest, sessRegion, "r.*", call, args...)
}

// callWithSessionColumns は callWithSession の、結果の選択リスト (既定 `r.*`) を差し替えられる版。
// リージョン違いでは関数が呼ばれず r の列が NULL になるので、NOT NULL の列を COALESCE で埋めれば
// 値の型 (string など) のまま受けられる (#17 PATCH /api/posts/:postId)。
func callWithSessionColumns(c *echo.Context, h *HandlerContext, dest any, sessRegion func() *string, columns, call string, args ...any) error {
	ctx := c.Request().Context()
	tokenHash, deferred := deferredSession(c)
	if !deferred {
		args = append(args, GetSession(c).UserID)
		query := `SELECT NULL::text AS sess_region, ` + columns + ` FROM ` + strings.ReplaceAll(call, "{user}", fmt.Sprintf("$%d", len(args))) + ` r`
		return h.DB.GetContext(ctx, dest, query, args...)
	}
	args = append(args, tokenHash, h.Config.Region)
	n := len(args)
	query := fmt.Sprintf(`
		SELECT s.sess_region, %s FROM (
			SELECT s.user_id AS sess_user_id, u.region AS sess_region
			FROM login_sessions s JOIN users u ON u.id = s.user_id
			WHERE s.token_hash = $%d AND s.active = true AND u.status = 'active'
		) s LEFT JOIN LATERAL (SELECT * FROM %s WHERE s.sess_region = $%d OFFSET 0) r ON true`,
		columns, n-1, strings.ReplaceAll(call, "{user}", "s.sess_user_id"), n)
	err := h.DB.GetContext(ctx, dest, query, args...)
	if errors.Is(err, sql.ErrNoRows) {
		c.Set(deferredSessionCheckedKey, true)
		knownSessions.Delete(tokenHash)
		c.Set("session", nil)
		return NewAPIError(http.StatusUnauthorized, "invalid_session")
	}
	if err != nil {
		return err
	}
	c.Set(deferredSessionCheckedKey, true)
	if region := sessRegion(); region == nil || *region != h.Config.Region {
		knownSessions.Delete(tokenHash)
		return NewAPIError(http.StatusForbidden, "region_mismatch")
	}
	return nil
}

// sessionSource は読み取りの 1 文に同居させるセッションの FROM 句の要素 `s` (sess_user_id, sess_region) を返す。
// deferred (覚えているセッションがある) なら login_sessions を引き、セッションが無効なら 0 行になる。
// そうでなければ SessionMiddleware で確認済みの利用者 id だけの 1 行 (sess_region は NULL = 確認済み)。
func sessionSource(c *echo.Context, args []any) (source string, sessionArgs []any, tokenHash string, deferred bool) {
	tokenHash, deferred = deferredSession(c)
	if !deferred {
		args = append(args, GetSession(c).UserID)
		return fmt.Sprintf(`(SELECT $%d::text AS sess_user_id, NULL::text AS sess_region) s`, len(args)), args, "", false
	}
	args = append(args, tokenHash)
	return fmt.Sprintf(`(SELECT s.user_id AS sess_user_id, u.region AS sess_region
		FROM login_sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $%d AND s.active = true AND u.status = 'active') s`, len(args)), args, tokenHash, true
}

// finishWithSession は 1 文で確認したセッションの結果 (行の有無と sess_region) を 401/403/nil にする。
// deferred でなければセッションは確認済みなので何もしない。
func finishWithSession(c *echo.Context, h *HandlerContext, tokenHash string, deferred, found bool, sessRegion *string) error {
	if !deferred {
		return nil
	}
	region := ""
	if sessRegion != nil {
		region = *sessRegion
	}
	return rejectDeferredSession(c, h, tokenHash, found, region)
}

// getWithSession は 1 行を返す読み取りの query を、セッションの確認と同じ 1 文で実行する (callWithSession の読み取り版)。
// query の {session} には sess_user_id と sess_region を持つ 1 行の `s` が入り、結果の行は sess_region 列を持つ。
// deferred のとき 0 行なら 401、sess_region がこのサーバと違えば 403。deferred でない 0 行は sql.ErrNoRows のまま返す。
func getWithSession(c *echo.Context, h *HandlerContext, dest any, sessRegion func() *string, query string, args ...any) error {
	source, args, tokenHash, deferred := sessionSource(c, args)
	err := h.DB.GetContext(c.Request().Context(), dest, strings.ReplaceAll(query, "{session}", source), args...)
	if deferred && errors.Is(err, sql.ErrNoRows) {
		return finishWithSession(c, h, tokenHash, deferred, false, nil)
	}
	if err != nil {
		return err
	}
	return finishWithSession(c, h, tokenHash, deferred, true, sessRegion())
}

// selectWithSession は複数行を返す読み取りの query を、セッションの確認と同じ 1 文で実行する。
// query は `{session} LEFT JOIN LATERAL (...) r ON true` の形にして、セッションが有効なら一覧が空でも
// 必ず 1 行 (r.* が NULL の番兵) を返すこと。rows は行数と先頭行の sess_region を返す。
// 0 行は deferred のときだけ起こり、セッションが無効 (401)。
// args の先頭に pgx.QueryExecMode を置くと、その文だけ実行モードを変える (セッションの $n の番号には数えない)。
func selectWithSession(c *echo.Context, h *HandlerContext, dest any, rows func() (int, *string), query string, args ...any) error {
	var options []any
	if len(args) > 0 {
		if mode, ok := args[0].(pgx.QueryExecMode); ok {
			options, args = []any{mode}, args[1:]
		}
	}
	source, args, tokenHash, deferred := sessionSource(c, args)
	if err := h.DB.SelectContext(c.Request().Context(), dest, strings.ReplaceAll(query, "{session}", source), append(options, args...)...); err != nil {
		return err
	}
	n, sessRegion := rows()
	return finishWithSession(c, h, tokenHash, deferred, n > 0, sessRegion)
}

// userColumnsCoalesced は LEFT JOIN LATERAL の番兵の行 (全列 NULL) を User にそのまま読めるよう、
// NOT NULL の列を空文字で埋めた選択リストを返す。profile_image_id は NULL を区別するのでそのまま。
// prefix が空なら列名そのまま、あれば alias."prefix.col" を読んで同じ名前で返す。
func userColumnsCoalesced(alias, prefix string) string {
	columns := make([]string, len(userColumns))
	for i, column := range userColumns {
		name := column
		if prefix != "" {
			name = fmt.Sprintf(`"%s.%s"`, prefix, column)
		}
		if column == "profile_image_id" {
			columns[i] = fmt.Sprintf(`%s.%s AS %s`, alias, name, name)
			continue
		}
		columns[i] = fmt.Sprintf(`COALESCE(%s.%s, '') AS %s`, alias, name, name)
	}
	return strings.Join(columns, ", ")
}
