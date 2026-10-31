package isureal

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"

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
		var rows []struct {
			User
			Stale   bool `db:"stale"`
			Matched bool `db:"matched"`
		}
		err := h.DB.SelectContext(ctx, &rows, `
  SELECT `+strings.Join(userColumns, ", ")+`, stale, matched FROM (
    SELECT u.*, m.stale, m.matched, count(*) FILTER (WHERE m.matched) OVER (ORDER BY u.username, u.id) AS n
    FROM users u
    LEFT JOIN user_search_norms s ON s.user_id = u.id
    CROSS JOIN LATERAL (SELECT s.display_name IS DISTINCT FROM u.display_name AS stale,
      COALESCE(s.display_name = u.display_name AND (strpos(s.username_norm, $4) > 0 OR strpos(s.display_name_norm, $4) > 0), false) AS matched) m
    WHERE u.status = 'active' AND u.id <> $1 AND ($2 = '' OR (u.username, u.id) > ($2, $3))
      AND NOT EXISTS (SELECT 1 FROM block_relations b
                      WHERE (b.blocker_id = $1 AND b.blocked_id = u.id) OR (b.blocked_id = $1 AND b.blocker_id = u.id))
  ) x
  WHERE (matched AND n <= 51) OR (stale AND n < 51)
  ORDER BY username, id
 `, viewer, input.BeforeUsername, input.BeforeID, query)
		if err != nil {
			return err
		}
		items := make([]relationshipUserResponse, 0)
		var staleUsers []User
		for _, row := range rows {
			user := row.User
			if row.Stale {
				staleUsers = append(staleUsers, user)
				if !strings.Contains(normalizeSearchText(user.Username), query) && !strings.Contains(normalizeSearchText(user.DisplayName), query) {
					continue
				}
			}
			if err := user.setImageKey(); err != nil {
				return err
			}
			if user.AccountType != "certified_public" {
				user.Bio = ""
			}
			items = append(items, newRelationshipUserResponse(user))
			if len(items) == 51 {
				break
			}
		}
		if len(staleUsers) > 0 {
			go saveSearchNorms(context.Background(), h.DB, staleUsers)
		}
		hasMore := len(items) > 50
		if hasMore {
			items = items[:50]
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
		ctx := c.Request().Context()
		viewer := GetSession(c).UserID
		var row struct {
			User
			Blocked  bool `db:"blocked"`
			IsFriend bool `db:"is_friend"`
		}
		err := h.DB.GetContext(ctx, &row, `
   SELECT `+strings.Join(userColumns, ", ")+`,
     EXISTS(SELECT 1 FROM block_relations WHERE (blocker_id=$1 AND blocked_id=u.id) OR (blocker_id=u.id AND blocked_id=$1)) AS blocked,
     EXISTS(SELECT 1 FROM friendships WHERE user_low_id=least($1, u.id) AND user_high_id=greatest($1, u.id)) AS is_friend
   FROM users u WHERE u.id = $2 AND u.status = 'active'
  `, viewer, input.UserID)
		if errors.Is(err, sql.ErrNoRows) {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if err != nil {
			return err
		}
		if row.Blocked {
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
	_, err := db.ExecContext(ctx, `
  INSERT INTO user_search_norms (user_id, username_norm, display_name, display_name_norm)
  SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[])
  ON CONFLICT (user_id) DO UPDATE SET username_norm = EXCLUDED.username_norm,
    display_name = EXCLUDED.display_name, display_name_norm = EXCLUDED.display_name_norm
 `, ids, usernameNorms, displayNames, displayNameNorms)
	return err
}

// refreshSearchNorms は正規化が無い・古い利用者をまとめて保存し直す (起動時と初期化後)。
func refreshSearchNorms(db *sqlx.DB) {
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, searchNormsSchema); err != nil {
		log.Printf("failed to create user_search_norms: %v", err)
		return
	}
	var users []User
	if err := db.SelectContext(ctx, &users, `
  SELECT u.id, u.username, u.display_name FROM users u
  LEFT JOIN user_search_norms s ON s.user_id = u.id
  WHERE s.display_name IS DISTINCT FROM u.display_name
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

const searchNormsSchema = `CREATE TABLE IF NOT EXISTS user_search_norms (
  user_id text PRIMARY KEY, username_norm text NOT NULL, display_name text NOT NULL, display_name_norm text NOT NULL)`
