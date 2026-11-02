package scenario

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"isureal-bench/internal/api"
	"isureal-bench/internal/client"
)

// 初期データが先の日付 (2026-11-07) まで投稿期間を持つ本番のような環境で、-start-date 無しの 1 日目が
// 409 を見て開ける日付まで進むこと (ci/bench/replica.sh は -manifest も -start-date も付けない)。
func TestOpenWindowAutoAdvancesPastConflicts(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/posting-window" || r.Method != http.MethodPost {
			rw.WriteHeader(http.StatusNotFound)
			return
		}
		var in struct {
			Region    string `json:"region"`
			LocalDate string `json:"localDate"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		asked = append(asked, in.LocalDate)
		mu.Unlock()
		rw.Header().Set("Content-Type", "application/json")
		if in.LocalDate <= "2026-11-07" { // app: SELECT EXISTS(... WHERE region = $1 AND local_date >= $2) → 409
			rw.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(rw).Encode(map[string]string{"error": "posting_window_conflict"})
			return
		}
		rw.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(rw).Encode(api.OpenPostingWindowResponse{ID: "w1", Region: in.Region, LocalDate: in.LocalDate,
			StartsAt: time.Now(), OnTimeDeadline: time.Now().Add(5 * time.Second)})
	}))
	defer srv.Close()

	w := New(DefaultConfig(), client.New(srv.URL, 5*time.Second), slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	w.admin = &client.Session{Country: "JP", ClientIP: "10.0.0.1"}
	rs := w.regions["asia"]
	rs.autoDate = true
	rs.lastLocalDate = time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC) // 「今日 - 1」。1 日目は 11-03 から

	res, got, err := w.openWindowAuto(context.Background(), rs, "asia", 1, "2026-11-03")
	if err != nil {
		t.Fatalf("openWindowAuto: %v (asked %v)", err, asked)
	}
	// 11-03 (409) → +1 = 11-04 (409) → +2 = 11-06 (409) → +4 = 11-10 (201)
	want := []string{"2026-11-03", "2026-11-04", "2026-11-06", "2026-11-10"}
	if len(asked) != len(want) {
		t.Fatalf("asked %v, want %v", asked, want)
	}
	for i := range want {
		if asked[i] != want[i] {
			t.Fatalf("asked %v, want %v", asked, want)
		}
	}
	if got != "2026-11-10" || res.LocalDate != got || rs.lastLocalDate.Format("2006-01-02") != got {
		t.Fatalf("localDate = %s, res = %+v, rs.lastLocalDate = %s", got, res, rs.lastLocalDate)
	}

	// 2 日目以降や -start-date 指定 (autoDate = false) では進めない: 409 はそのままエラー
	rs.autoDate = false
	if _, _, err := w.openWindowAuto(context.Background(), rs, "asia", 1, "2026-11-01"); !isConflict(err) {
		t.Fatalf("autoDate=false: err = %v, want 409", err)
	}
	rs.autoDate = true
	if _, _, err := w.openWindowAuto(context.Background(), rs, "asia", 2, "2026-11-01"); !isConflict(err) {
		t.Fatalf("day 2: err = %v, want 409", err)
	}
}
