package isureal

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

type getRelationshipsRequest struct {
	Kind            string     `param:"kind"`
	BeforeUpdatedAt *time.Time `query:"beforeUpdatedAt"`
	BeforeID        string     `query:"beforeId"`
}

type relationshipUserResponse struct {
	ID              string  `json:"id"`
	Username        string  `json:"username"`
	DisplayName     string  `json:"displayName"`
	ProfileImageURL *string `json:"profileImageUrl"`
	Bio             string  `json:"bio"`
	AccountType     string  `json:"accountType"`
}

type relationshipResponse struct {
	RelationID string                   `json:"relationId"`
	User       relationshipUserResponse `json:"user"`
	Status     string                   `json:"status"`
	UpdatedAt  time.Time                `json:"updatedAt"`
}

type getRelationshipsResponse struct {
	Items   []relationshipResponse `json:"items"`
	HasMore bool                   `json:"hasMore"`
}

func getRelationshipsHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input getRelationshipsRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		if (input.BeforeUpdatedAt == nil) != (input.BeforeID == "") {
			return NewAPIError(http.StatusBadRequest, "invalid_pagination")
		}
		// s.sess_user_id はセッションの利用者 (getWithSession / selectWithSession の {session})。
		var query string
		switch input.Kind {
		case "friends":
			query = `SELECT id, user_low_id AS first_id, user_high_id AS second_id, updated_at FROM friendships WHERE user_low_id=s.sess_user_id OR user_high_id=s.sess_user_id`
		case "following":
			query = `SELECT id, follower_id AS first_id, followee_id AS second_id, updated_at FROM follows WHERE follower_id=s.sess_user_id`
		case "followers":
			query = `SELECT id, follower_id AS first_id, followee_id AS second_id, updated_at FROM follows WHERE followee_id=s.sess_user_id`
		case "incoming-requests":
			query = `SELECT id, requester_id AS first_id, recipient_id AS second_id, updated_at FROM friend_requests WHERE recipient_id=s.sess_user_id AND status='pending'`
		case "outgoing-requests":
			query = `SELECT id, requester_id AS first_id, recipient_id AS second_id, updated_at FROM friend_requests WHERE requester_id=s.sess_user_id AND status='pending'`
		default:
			return NewAPIError(http.StatusBadRequest, "invalid_relationship")
		}
		// セッションの確認と一覧を 1 文で取る (#82)。一覧が空でも番兵の行 (found = false) が 1 行返る。
		var records []struct {
			SessRegion *string   `db:"sess_region"`
			Found      bool      `db:"found"`
			ID         string    `db:"id"`
			UpdatedAt  time.Time `db:"updated_at"`
			User       User      `db:"user"`
		}
		if err := selectWithSession(c, h, &records, func() (int, *string) {
			if len(records) == 0 {
				return 0, nil
			}
			return len(records), records[0].SessRegion
		}, `
   SELECT s.sess_region, r.id IS NOT NULL AS found, COALESCE(r.id, '') AS id, COALESCE(r.updated_at, 'epoch') AS updated_at,
          `+userColumnsCoalesced("r", "user")+`
   FROM {session} LEFT JOIN LATERAL (
     SELECT relations.id, relations.updated_at, `+userColumnsAs("u", "user")+`
     FROM (`+query+`) relations
     JOIN users u ON u.status = 'active'
                 AND u.id = CASE WHEN relations.first_id = s.sess_user_id THEN relations.second_id ELSE relations.first_id END
     WHERE ($1::timestamptz IS NULL OR (relations.updated_at, relations.id) < ($1, $2))
     ORDER BY relations.updated_at DESC, relations.id DESC LIMIT 51
   ) r ON true
   ORDER BY r.updated_at DESC, r.id DESC
  `, input.BeforeUpdatedAt, input.BeforeID); err != nil {
			return err
		}
		hasMore := len(records) > 50
		if hasMore {
			records = records[:50]
		}
		plan := newSignaturePlan(len(records))
		for i := range records {
			if records[i].Found {
				if err := records[i].User.setImageKey(); err != nil {
					return err
				}
				plan.addProfile(records[i].User)
			}
		}
		plan.run()
		items := make([]relationshipResponse, 0, len(records))
		for _, record := range records {
			if !record.Found {
				continue
			}
			user := record.User
			if user.AccountType != "certified_public" && input.Kind != "friends" {
				user.Bio = ""
			}
			status := input.Kind
			if input.Kind == "incoming-requests" || input.Kind == "outgoing-requests" {
				status = "pending"
			}
			items = append(items, relationshipResponse{
				RelationID: record.ID, Status: status, UpdatedAt: record.UpdatedAt,
				User: newRelationshipUserResponse(user),
			})
		}
		return c.JSON(http.StatusOK, getRelationshipsResponse{Items: items, HasMore: hasMore})
	}
}
