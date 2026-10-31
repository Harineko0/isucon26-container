package isureal

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v5"
)

type createFriendRequestRequest struct {
	RecipientID string `json:"recipientId"`
}

type createFriendRequestResponse struct {
	RelationID string    `json:"relationId"`
	Status     string    `json:"status"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

func createFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createFriendRequestRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		row, err := runRelationship(c, h, "friend_request", input.RecipientID)
		if err != nil {
			return err
		}
		request := FriendRequest{ID: *row.ID, Status: *row.Status, UpdatedAt: *row.UpdatedAt}
		return c.JSON(http.StatusCreated, createFriendRequestResponse{RelationID: request.ID, Status: request.Status, UpdatedAt: request.UpdatedAt})
	}
}

type transitionFriendRequestRequest struct {
	RequestID string `param:"requestId" json:"-"`
	Action    string `param:"action" json:"-"`
}

type transitionFriendRequestResponse createFriendRequestResponse

func transitionFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input transitionFriendRequestRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		var status string
		switch input.Action {
		case "accept":
			status = "accepted"
		case "reject":
			status = "rejected"
		case "cancel":
			status = "cancelled"
		default:
			// 未知の action は DB 関数を呼ばずに 403。ただし 401 → 403 の順を保つため、
			// セッションの確認を書き込みの文に後回しにしている (deferred) ときはここで確認する。
			if tokenHash, deferred := deferredSession(c); deferred {
				session, err := lookupSession(c.Request().Context(), h.DB, tokenHash)
				if err != nil {
					return err
				}
				region := ""
				if session != nil {
					region = session.Region
				}
				if err := rejectDeferredSession(c, h, tokenHash, session != nil, region); err != nil {
					return err
				}
			}
			return NewAPIError(http.StatusForbidden, "relationship_action_forbidden")
		}
		// セッションとリージョンの確認と同じ 1 文で呼ぶ (#17)。関数が呼ばれなかったときは o_* が NULL なのでポインタで受ける。
		var row struct {
			SessRegion *string    `db:"sess_region"`
			Result     *string    `db:"o_result"`
			ID         *string    `db:"o_id"`
			Status     *string    `db:"o_status"`
			UpdatedAt  *time.Time `db:"o_updated_at"`
		}
		if err := callWithSession(c, h, &row, func() *string { return row.SessRegion },
			`isureal_transition_friend_request_authorized({user}, $1, $2)`, input.RequestID, status); err != nil {
			return err
		}
		if row.Result == nil {
			return NewAPIError(http.StatusNotFound, "relationship_not_found")
		}
		if err := relationshipResultError(*row.Result); err != nil {
			return err
		}
		request := FriendRequest{ID: *row.ID, Status: *row.Status, UpdatedAt: *row.UpdatedAt}
		return c.JSON(http.StatusOK, transitionFriendRequestResponse{RelationID: request.ID, Status: request.Status, UpdatedAt: request.UpdatedAt})
	}
}
