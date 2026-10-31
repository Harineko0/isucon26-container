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
		var row sessionUserRow
		if err := h.DB.GetContext(ctx, &row, `SELECT * FROM isureal_login($1, $2, $3, $4)`,
			input.Username, hex.EncodeToString(hash[:]), uuid.NewString(), hex.EncodeToString(tokenHash[:])); err != nil {
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
		if err := InvalidateLoginSession(h.DB, session.UserID, session.TokenHash); err != nil {
			return err
		}
		return c.NoContent(http.StatusNoContent)
	}
}

func getMeHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		session := GetSession(c)
		if session == nil {
			return NewAPIError(http.StatusUnauthorized, "invalid_session")
		}
		user, err := getUser(c.Request().Context(), h.DB, session.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
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
		ctx := c.Request().Context()
		var user User
		err := h.DB.GetContext(ctx, &user, `
   UPDATE users SET display_name = COALESCE($2, display_name), bio = COALESCE($3, bio), region = COALESCE($4, region),
                    updated_at = clock_timestamp()
   WHERE id = $1
   RETURNING `+strings.Join(userColumns, ", "), session.UserID, input.DisplayName, input.Bio, input.Region)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && user.Status != "active") {
			return NewAPIError(http.StatusNotFound, "account_not_found")
		}
		if err != nil {
			return err
		}
		if err := user.setImageKey(); err != nil {
			return err
		}
		return c.JSON(http.StatusOK, updateMeResponse(newGetMeResponse(user)))
	}
}
