// Package api は ISUReal の API の JSON 形 (manual.md と app/webapp/nodejs の参照実装から)。
package api

import "time"

type ErrorResponse struct {
	Error string `json:"error"`
}

type InitializeResponse struct {
	Language string `json:"language"`
	Agent    string `json:"agent"`
	Harness  string `json:"harness"`
	Model    string `json:"model"`
}

type SessionUser struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	DisplayName string `json:"displayName"`
	Bio         string `json:"bio"`
	Region      string `json:"region"`
	AccountType string `json:"accountType"`
	Status      string `json:"status"`
}

type RegisterResponse struct {
	SecretKey string      `json:"secretKey"`
	User      SessionUser `json:"user"`
}

type LoginResponse struct {
	Token string      `json:"token"`
	User  SessionUser `json:"user"`
}

type Me struct {
	SessionUser
	ProfileImageURL *string `json:"profileImageUrl"`
}

type RelationshipUser struct {
	ID              string  `json:"id"`
	Username        string  `json:"username"`
	DisplayName     string  `json:"displayName"`
	ProfileImageURL *string `json:"profileImageUrl"`
	Bio             string  `json:"bio"`
	AccountType     string  `json:"accountType"`
}

type SearchUsersResponse struct {
	Items   []RelationshipUser `json:"items"`
	HasMore bool               `json:"hasMore"`
}

type Location struct {
	Kind      string   `json:"kind"`
	Latitude  *float64 `json:"latitude,omitempty"`
	Longitude *float64 `json:"longitude,omitempty"`
	Locality  *string  `json:"locality,omitempty"`
}

type Post struct {
	ID              string           `json:"id"`
	Author          RelationshipUser `json:"author"`
	PostingWindowID string           `json:"postingWindowId"`
	ImageURL        string           `json:"imageUrl"`
	Caption         string           `json:"caption"`
	Visibility      string           `json:"visibility"`
	Location        Location         `json:"location"`
	PostedAt        time.Time        `json:"postedAt"`
	OnTime          bool             `json:"onTime"`
	LateSeconds     int64            `json:"lateSeconds"`
	// friends-of-friends フィードだけ
	Score *int64 `json:"score,omitempty"`
}

type FeedResponse struct {
	Items   []Post `json:"items"`
	HasMore bool   `json:"hasMore"`
}

type PostingWindow struct {
	ID             string    `json:"id"`
	Region         string    `json:"region"`
	LocalDate      string    `json:"localDate"`
	StartsAt       time.Time `json:"startsAt"`
	OnTimeDeadline time.Time `json:"onTimeDeadline"`
}

type CurrentPostingWindowResponse struct {
	Window        *PostingWindow `json:"window"`
	HasPosted     bool           `json:"hasPosted"`
	CanPost       bool           `json:"canPost"`
	DeletionCount int            `json:"deletionCount"`
}

type RealMoji struct {
	ID           string           `json:"id"`
	Reactor      RelationshipUser `json:"reactor"`
	ReactionType string           `json:"reactionType"`
	ImageURL     string           `json:"imageUrl"`
	CreatedAt    time.Time        `json:"createdAt"`
}

type RealMojisResponse struct {
	Items   []RealMoji `json:"items"`
	HasMore bool       `json:"hasMore"`
}

type Comment struct {
	ID        string           `json:"id"`
	Author    RelationshipUser `json:"author"`
	Body      string           `json:"body"`
	CreatedAt time.Time        `json:"createdAt"`
}

type CommentsResponse struct {
	Items   []Comment `json:"items"`
	HasMore bool      `json:"hasMore"`
}

type MemoryPost struct {
	Post
	RealMojis []RealMoji `json:"realmojis"`
	Comments  []Comment  `json:"comments"`
}

type MemoriesResponse struct {
	Items   []MemoryPost `json:"items"`
	HasMore bool         `json:"hasMore"`
}

type ExportPost struct {
	Post
	ImagePath string `json:"imagePath"`
}

type Notification struct {
	ID              string    `json:"id"`
	PostingWindowID string    `json:"postingWindowId"`
	Region          string    `json:"region"`
	CreatedAt       time.Time `json:"createdAt"`
}

type FriendRequestResponse struct {
	RelationID string    `json:"relationId"`
	Status     string    `json:"status"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type Relationship struct {
	RelationID string           `json:"relationId"`
	User       RelationshipUser `json:"user"`
	Status     string           `json:"status"`
	UpdatedAt  time.Time        `json:"updatedAt"`
}

type RelationshipsResponse struct {
	Items   []Relationship `json:"items"`
	HasMore bool           `json:"hasMore"`
}

type AdminLoginResponse struct {
	Token           string `json:"token"`
	AdministratorID string `json:"administratorId"`
}

type OpenPostingWindowResponse struct {
	ID             string    `json:"id"`
	Region         string    `json:"region"`
	LocalDate      string    `json:"localDate"`
	StartsAt       time.Time `json:"startsAt"`
	OnTimeDeadline time.Time `json:"onTimeDeadline"`
}

// CountryCodes はルーターがリージョンに振り分ける国コード (manual.md「通信経路」)。
var CountryCodes = map[string][]string{
	"asia":    {"JP", "KR", "CN", "AU"},
	"europe":  {"GB", "DE", "FR"},
	"america": {"US", "CA", "MX"},
}

var Regions = []string{"asia", "europe", "america"}

// Multiplier はリージョンの倍率 (manual.md「スコアの計算」)。
func Multiplier(region string) float64 {
	switch region {
	case "europe", "america":
		return 1.5
	default:
		return 1
	}
}
