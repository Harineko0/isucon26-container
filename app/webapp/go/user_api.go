package isureal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type registerRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Region      string `json:"region"`
}

type registerResponse struct {
	SecretKey string              `json:"secretKey"`
	User      sessionUserResponse `json:"user"`
}

type loginRequest struct {
	Username  string `json:"username"`
	SecretKey string `json:"secretKey"`
}

type loginResponse struct {
	Token string              `json:"token"`
	User  sessionUserResponse `json:"user"`
}

type sessionUserResponse struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Bio         string `json:"bio"`
	Region      string `json:"region"`
	AccountType string `json:"accountType"`
	Status      string `json:"status"`
}

type getMeResponse struct {
	sessionUserResponse
	ProfileImageURL *string `json:"profileImageUrl"`
}

type updateMeRequest struct {
	DisplayName *string `json:"displayName"`
	Bio         *string `json:"bio"`
	Region      *string `json:"region"`
}

type updateMeResponse getMeResponse

type sessionUserRow struct {
	Result      string  `db:"o_result"`
	ID          *string `db:"o_id"`
	Username    *string `db:"o_username"`
	DisplayName *string `db:"o_display_name"`
	Bio         *string `db:"o_bio"`
	Region      *string `db:"o_region"`
	AccountType *string `db:"o_account_type"`
	Status      *string `db:"o_status"`
}

// isureal_update_me の 1 行。関数が呼ばれなかったときは o_* が NULL なのでポインタで受ける。
type updateMeRow struct {
	SessRegion     *string `db:"sess_region"`
	Result         *string `db:"o_result"`
	ID             *string `db:"o_id"`
	Username       *string `db:"o_username"`
	SecretKeyHash  *string `db:"o_secret_key_hash"`
	DisplayName    *string `db:"o_display_name"`
	Bio            *string `db:"o_bio"`
	ProfileImageID *string `db:"o_profile_image_id"`
	Region         *string `db:"o_region"`
	AccountType    *string `db:"o_account_type"`
	Status         *string `db:"o_status"`
}

func (row sessionUserRow) user() User {
	return User{ID: *row.ID, Username: *row.Username, DisplayName: *row.DisplayName, Bio: *row.Bio,
		Region: *row.Region, AccountType: *row.AccountType, Status: *row.Status}
}

func newSessionUserResponse(user User) sessionUserResponse {
	return sessionUserResponse{
		ID: user.ID, Username: user.Username, DisplayName: user.DisplayName,
		Bio: user.Bio, Region: user.Region, AccountType: user.AccountType, Status: user.Status,
	}
}

func newGetMeResponse(user User) getMeResponse {
	response := getMeResponse{sessionUserResponse: newSessionUserResponse(user)}
	if user.ProfileImageID != nil {
		url := signedImageURL(user, *user.ProfileImageID)
		response.ProfileImageURL = &url
	}
	return response
}

func registerHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input registerRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		c.Set("username", input.Username)
		if !usernamePattern.MatchString(input.Username) || !validUserText(input.DisplayName, 1, 100) || !validRegion(input.Region) {
			return NewAPIError(http.StatusBadRequest, "invalid_registration")
		}
		secretKey := uuid.NewString()
		hash := sha256.Sum256([]byte(secretKey))
		ctx := c.Request().Context()
		var row sessionUserRow
		if err := h.DB.GetContext(ctx, &row, `SELECT * FROM isureal_register($1, $2, $3, $4, $5)`,
			uuid.NewString(), input.Username, hex.EncodeToString(hash[:]), input.DisplayName, input.Region); err != nil {
			return err
		}
		if row.Result != "ok" {
			return NewAPIError(http.StatusConflict, "username_taken")
		}
		user := row.user()
		saveSearchNormsAsync(h.DB, user)
		return c.JSON(http.StatusCreated, registerResponse{SecretKey: secretKey, User: newSessionUserResponse(user)})
	}
}

func loginHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input loginRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		c.Set("username", input.Username)
		if !usernamePattern.MatchString(input.Username) {
			return NewAPIError(http.StatusUnauthorized, "invalid_credentials")
		}
		ctx := c.Request().Context()
		hash := sha256.Sum256([]byte(input.SecretKey))
		token := uuid.NewString()
		tokenHash := sha256.Sum256([]byte(token))
		conn, err := h.IPBans.connUnlessBanned(ctx, h.DB, c.RealIP())
		if err != nil {
			return err
		}
		var row sessionUserRow
		err = conn.GetContext(ctx, &row, `SELECT * FROM isureal_login($1, $2, $3, $4)`,
			input.Username, hex.EncodeToString(hash[:]), uuid.NewString(), hex.EncodeToString(tokenHash[:]))
		conn.Close()
		if err != nil {
			return err
		}
		if row.Result != "ok" {
			return NewAPIError(http.StatusUnauthorized, "invalid_credentials")
		}
		user := row.user()
		session := &Session{Token: token}
		knownSessions.Store(hex.EncodeToString(tokenHash[:]), Session{UserID: user.ID, TokenHash: hex.EncodeToString(tokenHash[:]), Region: user.Region})
		return c.JSON(http.StatusOK, loginResponse{Token: session.Token, User: newSessionUserResponse(user)})
	}
}

func logoutHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		// InvalidateLoginSession と同じ処理を、セッションの確認と同じ 1 文で行う (#17)。
		// スカラーを返す関数は (SELECT f(...) AS result) で包む: 素の `FROM f(...) r` だと列名が r になり、
		// deferred の LATERAL 形 (列名が関数名) と一致しない。
		knownSessions.Delete(session.TokenHash)
		var row struct {
			SessRegion *string `db:"sess_region"`
			Result     *string `db:"result"`
		}
		if err := callWithSessionAnyRegion(c, h, &row, `(SELECT isureal_logout({user}, $1) AS result)`, session.TokenHash); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

func getMeHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		if GetSession(c) == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		// セッションの確認と利用者の取得を 1 文で行う (#82)。
		var row struct {
			SessRegion *string `db:"sess_region"`
			User
		}
		err := getWithSession(c, h, &row, func() *string { return row.SessRegion }, `
   SELECT s.sess_region, u.`+strings.Join(userColumns, ", u.")+`
   FROM {session} JOIN users u ON u.id = s.sess_user_id AND u.status = 'active'
  `)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
			return err
		}
		user := row.User
		if err := user.setImageKey(); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, newGetMeResponse(user))
	}
}

func updateMeHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		var input updateMeRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.DisplayName == nil && input.Bio == nil && input.Region == nil) ||
			(input.DisplayName != nil && !validUserText(*input.DisplayName, 1, 100)) ||
			(input.Bio != nil && !validUserText(*input.Bio, 0, 500)) ||
			(input.Region != nil && !validRegion(*input.Region)) {
			return NewAPIError(http.StatusBadRequest, "invalid_profile")
		}
		// UPDATE users ... RETURNING を isureal_update_me にして、セッションの確認と同じ 1 文で行う (#17)。
		// このルートにはリージョンの確認が無い (リージョンを変える操作でもある) ので callWithSessionAnyRegion。
		var row updateMeRow
		if err := callWithSessionAnyRegion(c, h, &row, `isureal_update_me({user}, $1, $2, $3)`, input.DisplayName, input.Bio, input.Region); err != nil {
			return err
		}
		if row.Result == nil || *row.Result != "ok" || *row.Status != "active" {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		user := User{ID: *row.ID, Username: *row.Username, SecretKeyHash: *row.SecretKeyHash, DisplayName: *row.DisplayName, Bio: *row.Bio,
			ProfileImageID: row.ProfileImageID, Region: *row.Region, AccountType: *row.AccountType, Status: *row.Status}
		if input.DisplayName != nil {
			saveSearchNormsAsync(h.DB, user)
		}
		if err := user.setImageKey(); err != nil {
			return err
		}
		// リージョンが変わっていることがあるので、覚えているセッションも更新する。
		knownSessions.Store(session.TokenHash, Session{UserID: user.ID, TokenHash: session.TokenHash, CreatedAt: session.CreatedAt, Region: user.Region})
		return c.JSON(http.StatusOK, updateMeResponse(newGetMeResponse(user)))
	}
}
