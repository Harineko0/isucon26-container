package isureal

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// 登録・表示名の変更のあとの正規化の保存をまとめて 1 文にする。1 人ずつ保存すると、ベンチ 1 回で
// 約 1,300 文・平均 44ms (DB の CPU が 100% のとき) になっていた。保存までの間はトリガーが入れた
// user_search_dirty の行があるので、検索はアプリで照合する (#26 #114)。
const (
	searchNormsBatchMax  = 200
	searchNormsBatchWait = 100 * time.Millisecond
	searchNormsQueueSize = 4096
	searchNormsSaveLimit = 5 * time.Second
)

// searchNormsSaver は正規化の保存を予約順にまとめて保存する。run を 1 つのゴルーチンで動かす。
type searchNormsSaver struct {
	db    *sqlx.DB
	queue chan User
}

func newSearchNormsSaver(db *sqlx.DB) *searchNormsSaver {
	return &searchNormsSaver{db: db, queue: make(chan User, searchNormsQueueSize)}
}

// enqueue は保存を予約する。応答は待たせない。キューが満杯なら、その利用者は dirty に残る
// (検索はその利用者をアプリで照合し、結果に出たときに保存する)。
func (s *searchNormsSaver) enqueue(user User) {
	select {
	case s.queue <- user:
	default:
		slog.Warn("search norms queue full; user stays in user_search_dirty", "user_id", user.ID)
	}
}

// run は ctx が終わるまで、まとめた分ずつ保存する。
func (s *searchNormsSaver) run(ctx context.Context) {
	for {
		batch := collectSearchNormsBatch(ctx, s.queue, searchNormsBatchMax, searchNormsBatchWait)
		if batch == nil {
			return
		}
		saveCtx, cancel := context.WithTimeout(ctx, searchNormsSaveLimit)
		if err := saveSearchNorms(saveCtx, s.db, batch); err != nil {
			// バックグラウンドの保存の境界なので、ここで記録して次へ進む (利用者は dirty に残る)。
			slog.Error("save search norms", "users", len(batch), "err", err)
		}
		cancel()
	}
}

// collectSearchNormsBatch は最初の 1 件を待ち、そこから wait の間か max 件までをまとめる。
// 同じ利用者が複数あれば後のもの (新しい表示名) を残し、順序は最初に現れた順にする。
// ON CONFLICT DO UPDATE は 1 文の中で同じ行を 2 回更新できないので、重複は必ず除く。
// ctx が終わったか ch が閉じられて空なら nil を返す。
func collectSearchNormsBatch(ctx context.Context, ch <-chan User, max int, wait time.Duration) []User {
	var first User
	select {
	case <-ctx.Done():
		return nil
	case u, ok := <-ch:
		if !ok {
			return nil
		}
		first = u
	}
	order := []string{first.ID}
	byID := map[string]User{first.ID: first}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for len(order) < max {
		select {
		case <-ctx.Done():
			return orderedUsers(order, byID)
		case <-timer.C:
			return orderedUsers(order, byID)
		case u, ok := <-ch:
			if !ok {
				return orderedUsers(order, byID)
			}
			if _, seen := byID[u.ID]; !seen {
				order = append(order, u.ID)
			}
			byID[u.ID] = u
		}
	}
	return orderedUsers(order, byID)
}

func orderedUsers(order []string, byID map[string]User) []User {
	users := make([]User, len(order))
	for i, id := range order {
		users[i] = byID[id]
	}
	return users
}

var (
	searchNormsSaverOnce sync.Once
	searchNormsSaverInst *searchNormsSaver
)

// saveSearchNormsAsync は登録・表示名の変更のあとに正規化の保存を予約する (呼び出し側は変えない)。
// 保存のゴルーチンはプロセスと同じ寿命で、最初の呼び出しで 1 つだけ起動する。
func saveSearchNormsAsync(db *sqlx.DB, user User) {
	searchNormsSaverOnce.Do(func() {
		searchNormsSaverInst = newSearchNormsSaver(db)
		go searchNormsSaverInst.run(context.Background())
	})
	searchNormsSaverInst.enqueue(user)
}
