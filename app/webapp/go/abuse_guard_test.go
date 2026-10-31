package isureal

import (
	"testing"
	"time"
)

func TestAbuseKind(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		action string
		status int
		abuse  string
		want   string
	}{
		{name: "login failure", action: "login", status: 401, want: "login"},
		{name: "login success", action: "login", status: 200, want: ""},
		{name: "login banned", action: "login", status: 403, want: ""},
		{name: "invalid image", action: "put_profile_image", status: 400, abuse: "invalid_image", want: "invalid_image"},
		{name: "bad request without abuse flag", action: "put_profile_image", status: 400, want: ""},
		{name: "invalid signature", action: "get_image", status: 403, abuse: "invalid_signature", want: "invalid_signature"},
		{name: "forbidden image without abuse flag", action: "get_image", status: 403, want: ""},
		{name: "unmapped route", action: "", status: 401, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := abuseKind(tt.action, tt.status, tt.abuse); got != tt.want {
				t.Errorf("abuseKind(%q, %d, %q) = %q, want %q", tt.action, tt.status, tt.abuse, got, tt.want)
			}
		})
	}
}

// guard と同じ回数・期間で BAN することを確かめる。
func TestAbuseDetectorThresholds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		kind      string
		threshold int
		window    time.Duration
	}{
		{kind: "login", threshold: 10, window: 10 * time.Second},
		{kind: "invalid_image", threshold: 2, window: 5 * time.Second},
		{kind: "invalid_signature", threshold: 3, window: 5 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			t.Parallel()
			base := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
			step := tt.window / time.Duration(tt.threshold*2)

			d := newAbuseDetector()
			for i := 0; i < tt.threshold-1; i++ {
				if d.observe(tt.kind, "1.2.3.4", base.Add(time.Duration(i)*step)) {
					t.Fatalf("banned after %d failures, want %d", i+1, tt.threshold)
				}
			}
			last := base.Add(time.Duration(tt.threshold-1) * step)
			if d.bannedAt("1.2.3.4", last) {
				t.Fatal("banned before reaching the threshold")
			}
			if !d.observe(tt.kind, "1.2.3.4", last) {
				t.Fatalf("not banned after %d failures", tt.threshold)
			}
			if !d.bannedAt("1.2.3.4", last.Add(time.Millisecond)) {
				t.Fatal("ban is not effective right away")
			}
			if d.bannedAt("5.6.7.8", last) {
				t.Fatal("another IP is banned")
			}
			if got := <-d.pending; got != "1.2.3.4" {
				t.Fatalf("ban queued for %q, want 1.2.3.4", got)
			}

			// 期間を外れた失敗は数えない。
			d = newAbuseDetector()
			for i := 0; i < tt.threshold-1; i++ {
				d.observe(tt.kind, "1.2.3.4", base)
			}
			if d.observe(tt.kind, "1.2.3.4", base.Add(tt.window)) {
				t.Fatal("failures outside the window were counted")
			}
		})
	}
}

func TestAbuseDetectorKindsAreSeparate(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	d := newAbuseDetector()
	// 種類ごとに閾値の 1 回手前まで: 合計では閾値を超えるが BAN しない。
	for i := 0; i < 9; i++ {
		d.observe("login", "1.2.3.4", base)
	}
	d.observe("invalid_image", "1.2.3.4", base)
	d.observe("invalid_signature", "1.2.3.4", base)
	d.observe("invalid_signature", "1.2.3.4", base)
	if d.bannedAt("1.2.3.4", base) {
		t.Fatal("failures of different kinds were added together")
	}
}

func TestAbuseDetectorLocalBanExpiresAndResets(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

	d := newAbuseDetector()
	d.observe("invalid_image", "1.2.3.4", base)
	d.observe("invalid_image", "1.2.3.4", base)
	if !d.bannedAt("1.2.3.4", base.Add(localBanTTL-time.Millisecond)) {
		t.Fatal("local ban ended too early")
	}
	// その後は ip_bans の再読込に任せる (initialize の TRUNCATE が全リージョンに効くように)。
	if d.bannedAt("1.2.3.4", base.Add(localBanTTL)) {
		t.Fatal("local ban outlived localBanTTL")
	}

	d.observe("invalid_image", "1.2.3.4", base)
	d.observe("invalid_image", "1.2.3.4", base)
	d.observe("invalid_signature", "5.6.7.8", base)
	d.observe("invalid_signature", "5.6.7.8", base)
	d.reset()
	if d.bannedAt("1.2.3.4", base) {
		t.Fatal("local ban survived reset")
	}
	if d.observe("invalid_signature", "5.6.7.8", base) {
		t.Fatal("failures counted before reset were kept")
	}
}

func TestAbuseDetectorPendingQueueFullDoesNotBlock(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	d := newAbuseDetector()
	d.pending = make(chan string) // 受け手なし
	d.observe("invalid_image", "1.2.3.4", base)
	if !d.observe("invalid_image", "1.2.3.4", base) {
		t.Fatal("not banned")
	}
	if !d.bannedAt("1.2.3.4", base) {
		t.Fatal("local ban missing when the write queue is full")
	}
}
