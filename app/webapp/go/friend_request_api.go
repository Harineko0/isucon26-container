package isureal

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
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

const transitionFriendRequestCall = `isureal_transition_friend_request_authorized({user}, $1, $2)`

var friendRequestSQL = deferredMutationSQL("r.*", `isureal_relationship('friend_request', {user}, $1, $2)`, 2)
var transitionFriendRequestSQL = deferredMutationSQL("r.*", transitionFriendRequestCall, 2)

func createFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
	return createFriendRequestHandlerWithQueue(h, &writeQueue[relationshipResult]{db: h.DB, sql: friendRequestSQL})
}

func createFriendRequestHandlerWithQueue(h *HandlerContext, writes *writeQueue[relationshipResult]) echo.HandlerFunc {
	return func(c *echo.Context) error {
		var input createFriendRequestRequest
		if err := c.Bind(&input); err != nil {
			return err
		}
		row, err := createFriendRequest(c, h, writes, input.RecipientID)
		if err != nil {
			return err
		}
		// The pipeline receives text timestamps in the DB session's zone. Match
		// the local zone used by pgx for a prepared binary timestamptz.
		request := FriendRequest{ID: *row.ID, Status: *row.Status, UpdatedAt: row.UpdatedAt.Local()}
		return c.JSON(http.StatusCreated, createFriendRequestResponse{RelationID: request.ID, Status: request.Status, UpdatedAt: request.UpdatedAt})
	}
}

func createFriendRequest(c *echo.Context, h *HandlerContext, writes *writeQueue[relationshipResult], recipient string) (relationshipResult, error) {
	token, deferred := deferredSession(c)
	if !deferred || recipient == "" || recipient == GetSession(c).UserID {
		return runRelationship(c, h, "friend_request", recipient)
	}
	row, err := queuedFriendRequest(c, h, writes, token, recipient, uuid.NewString())
	if err != nil {
		return row, err
	}
	if row.Result == nil {
		return row, errors.New("relationship function returned no result")
	}
	return row, relationshipResultError(*row.Result)
}

func queuedFriendRequest(c *echo.Context, h *HandlerContext, writes *writeQueue[relationshipResult], token, first, second string) (relationshipResult, error) {
	row, err := writes.execute(c.Request().Context(), "", first, second, token, h.Config.Region)
	return row, finishDeferredMutation(c, h, token, row.SessRegion, err)
}

type transitionFriendRequestRequest struct {
	RequestID string `param:"requestId" json:"-"`
	Action    string `param:"action" json:"-"`
}

type transitionFriendRequestResponse createFriendRequestResponse

func transitionFriendRequestHandler(h *HandlerContext) echo.HandlerFunc {
	return transitionFriendRequestHandlerWithQueue(h, &writeQueue[relationshipResult]{db: h.DB, sql: transitionFriendRequestSQL})
}

func transitionFriendRequestHandlerWithQueue(h *HandlerContext, writes *writeQueue[relationshipResult]) echo.HandlerFunc {
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
		var row relationshipResult
		var err error
		if token, deferred := deferredSession(c); deferred {
			row, err = queuedFriendRequest(c, h, writes, token, input.RequestID, status)
		} else {
			err = callWithSession(c, h, &row, func() *string { return row.SessRegion }, transitionFriendRequestCall, input.RequestID, status)
		}
		if err != nil {
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
