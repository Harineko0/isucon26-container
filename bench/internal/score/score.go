// Package score は manual.md「スコアの計算」の配点表と集計。
// スコア = Σ(採点件数 × 配点 × リージョンの倍率)、小数点以下切り捨て。
package score

import (
	"math"
	"sort"
	"sync"

	"isureal-bench/internal/api"
)

type Kind string

const (
	FeedVerify         Kind = "post_feed_verified"       // 投稿後の友達フィードの検証通過（通常）
	FeedVerifyFinal    Kind = "post_feed_verified_final" // 同（最後の投稿期間）
	FeedView           Kind = "feed_view"                // フィードの閲覧
	ImageGet           Kind = "image_get"                // 画像の取得
	PostGet            Kind = "post_get"                 // 投稿詳細の取得
	InteractionList    Kind = "interaction_list"         // RealMoji・コメント一覧の取得
	ProfileView        Kind = "profile_view"             // プロフィールの閲覧
	Notification       Kind = "notification"             // 通知の受信
	MemoryPrevious     Kind = "memory_previous_post"     // Memories で前回の投稿を確認
	RealMojiSend       Kind = "realmoji_send"            // RealMoji の送信
	CommentPost        Kind = "comment_post"             // コメントの投稿
	InteractionDelete  Kind = "interaction_delete"       // RealMoji・コメントの削除
	UserSearch         Kind = "user_search"              // ユーザーの検索
	FriendRequest      Kind = "friend_request"           // 友達申請の送信
	FriendAccept       Kind = "friend_accept"            // 友達申請の承認
	FriendRejectCancel Kind = "friend_reject_cancel"     // 友達申請の拒否・取り下げ
	Unfriend           Kind = "unfriend"                 // 友達の解除
	RelationshipList   Kind = "relationship_list"        // 友達・フォロー・フォロワーの一覧取得
	Follow             Kind = "follow"                   // フォロー
	Unfollow           Kind = "unfollow"                 // フォローの解除
	Block              Kind = "block"                    // ブロック
	Unblock            Kind = "unblock"                  // ブロックの解除
	ProfileUpdate      Kind = "profile_update"           // 表示名・自己紹介の変更
	ProfileImage       Kind = "profile_image"            // プロフィール画像の変更・削除
	PostUpdate         Kind = "post_update"              // 投稿のキャプションの変更
	AccountDeletion    Kind = "account_deletion"         // 退会
	MemoriesMonth      Kind = "memories_month"           // Memories を月単位で閲覧
	MemoriesDate       Kind = "memories_date"            // Memories を日付単位で閲覧
	Export             Kind = "export"                   // エクスポート
	PastImageGet       Kind = "past_image_get"           // 過去の投稿の画像の取得
)

var Points = map[Kind]int{
	FeedVerify: 10, FeedVerifyFinal: 20, FeedView: 1, ImageGet: 2, PostGet: 1, InteractionList: 1, ProfileView: 1,
	Notification: 1, MemoryPrevious: 1, RealMojiSend: 5, CommentPost: 3, InteractionDelete: 1, UserSearch: 2,
	FriendRequest: 2, FriendAccept: 2, FriendRejectCancel: 1, Unfriend: 1, RelationshipList: 1, Follow: 1, Unfollow: 1,
	Block: 1, Unblock: 1, ProfileUpdate: 1, ProfileImage: 3, PostUpdate: 1, AccountDeletion: 5, MemoriesMonth: 3,
	MemoriesDate: 2, Export: 20, PastImageGet: 2,
}

// 投稿期間ごとの閲覧中の加点上限 (利用者 1 人あたり)。manual.md「スコアの計算」。
var WindowCaps = map[Kind]int{
	FeedView: 20, ImageGet: 4, PostGet: 8, InteractionList: 8, ProfileView: 4, RealMojiSend: 5, CommentPost: 5,
}

type Board struct {
	mu     sync.Mutex
	total  float64
	counts map[Kind]int64
	points map[Kind]float64
	region map[string]float64
}

func NewBoard() *Board {
	return &Board{counts: map[Kind]int64{}, points: map[Kind]float64{}, region: map[string]float64{}}
}

// Add は 1 件を加点する。region は加点した時点の利用者の所属リージョン。
func (b *Board) Add(kind Kind, region string) {
	p := float64(Points[kind]) * api.Multiplier(region)
	b.mu.Lock()
	b.total += p
	b.counts[kind]++
	b.points[kind] += p
	b.region[region] += p
	b.mu.Unlock()
}

func (b *Board) Total() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int64(math.Floor(b.total))
}

type Line struct {
	Kind   Kind    `json:"kind"`
	Count  int64   `json:"count"`
	Points float64 `json:"points"`
}

func (b *Board) Breakdown() []Line {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Line, 0, len(b.counts))
	for k, n := range b.counts {
		out = append(out, Line{Kind: k, Count: n, Points: b.points[k]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Points > out[j].Points })
	return out
}

func (b *Board) ByRegion() map[string]float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]float64{}
	for k, v := range b.region {
		out[k] = v
	}
	return out
}

func (b *Board) Count(kind Kind) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.counts[kind]
}

// WindowCounter は利用者 1 人の、1 つの投稿期間の中での加点回数 (上限の判定)。
type WindowCounter struct {
	mu     sync.Mutex
	counts map[Kind]int
}

func NewWindowCounter() *WindowCounter { return &WindowCounter{counts: map[Kind]int{}} }

// Take は上限内なら 1 回ぶん消費して true を返す。上限が無い種別は常に true。
func (c *WindowCounter) Take(kind Kind) bool {
	cap, capped := WindowCaps[kind]
	if !capped {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.counts[kind] >= cap {
		return false
	}
	c.counts[kind]++
	return true
}

// Remaining は残りの加点回数 (上限が無ければ -1)。
func (c *WindowCounter) Remaining(kind Kind) int {
	cap, capped := WindowCaps[kind]
	if !capped {
		return -1
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return cap - c.counts[kind]
}
