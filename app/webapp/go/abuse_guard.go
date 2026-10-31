package isureal

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
)

// isureal-guard (/opt/isureal-guard) は action log を journald 経由で読んで ip_bans に書くので、
// BAN が効くまで約 1 秒かかり、その間も攻撃のリクエストを DB を使って処理していた (#109)。
// アプリでも guard と同じ条件で数え、閾値に達したらすぐ BAN する。条件・期間・SQL は guard と同じ。
// guard はそのまま動かす (同じ BAN を書くだけで、GREATEST なので結果は変わらない)。

type abuseRule struct {
	threshold int
	window    time.Duration
}

// abuseRules は /opt/isureal-guard/src/guard.go と isureal-guard.service の値。
var abuseRules = map[string]abuseRule{
	"login":             {threshold: 10, window: 10 * time.Second},
	"invalid_image":     {threshold: 2, window: 5 * time.Second},
	"invalid_signature": {threshold: 3, window: 5 * time.Second},
}

const (
	ipBanDuration = 2 * time.Minute
	// localBanTTL の間はアプリ内の BAN を使い、その後は ip_bans (200ms ごとの再読込) を正とする。
	// こうしておくと initialize の TRUNCATE ip_bans は従来どおり全リージョンに反映される。
	localBanTTL = 3 * time.Second
)

// abuseKind は guard の parseFailure と同じ判定で、action log の 1 行が BAN の数に入るかを返す。
func abuseKind(action string, status int, abuse string) string {
	switch {
	case action == "login" && status == 401:
		return "login"
	case action == "put_profile_image" && status == 400 && abuse == "invalid_image":
		return "invalid_image"
	case action == "get_image" && status == 403 && abuse == "invalid_signature":
		return "invalid_signature"
	}
	return ""
}

// abuseDetector は IP ごとの失敗の時刻を種類別に持つ (guard の detector と同じ数え方)。
type abuseDetector struct {
	mu        sync.Mutex
	failures  map[string]map[string][]time.Time // kind -> ip -> 時刻
	lastSweep map[string]time.Time
	local     sync.Map // ip -> time.Time (アプリ内の BAN の期限)
	pending   chan string
}

func newAbuseDetector() *abuseDetector {
	return &abuseDetector{
		failures:  make(map[string]map[string][]time.Time),
		lastSweep: make(map[string]time.Time),
		pending:   make(chan string, 1024),
	}
}

// observe は失敗を 1 件数え、閾値に達したらその IP をすぐ BAN して true を返す。
func (d *abuseDetector) observe(kind, ip string, at time.Time) bool {
	rule, ok := abuseRules[kind]
	if !ok || ip == "" {
		return false
	}
	d.mu.Lock()
	byIP := d.failures[kind]
	if byIP == nil {
		byIP = make(map[string][]time.Time)
		d.failures[kind] = byIP
	}
	if at.Sub(d.lastSweep[kind]) >= rule.window {
		d.lastSweep[kind] = at
		for other, times := range byIP {
			if at.Sub(times[len(times)-1]) >= rule.window {
				delete(byIP, other)
			}
		}
	}
	kept := byIP[ip][:0]
	for _, failedAt := range byIP[ip] {
		if at.Sub(failedAt) < rule.window {
			kept = append(kept, failedAt)
		}
	}
	kept = append(kept, at)
	banned := len(kept) >= rule.threshold
	if banned {
		delete(byIP, ip)
	} else {
		byIP[ip] = kept
	}
	d.mu.Unlock()
	if banned {
		d.local.Store(ip, at.Add(localBanTTL))
		select {
		case d.pending <- ip:
		default: // 詰まっていれば guard の書き込みに任せる
		}
	}
	return banned
}

func (d *abuseDetector) bannedAt(ip string, now time.Time) bool {
	until, ok := d.local.Load(ip)
	if !ok {
		return false
	}
	if now.Before(until.(time.Time)) {
		return true
	}
	d.local.CompareAndDelete(ip, until)
	return false
}

// reset は initialize で呼ぶ。数えた失敗とアプリ内の BAN を捨てる。
func (d *abuseDetector) reset() {
	d.mu.Lock()
	d.failures = make(map[string]map[string][]time.Time)
	d.lastSweep = make(map[string]time.Time)
	d.mu.Unlock()
	d.local.Clear()
}

// writeLoop は BAN を ip_bans に書き、他のサーバーにも伝える (guard と同じ SQL)。
func (d *abuseDetector) writeLoop(db *sqlx.DB) {
	for ip := range d.pending {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := db.ExecContext(ctx, `
  INSERT INTO ip_bans (ip, banned_until)
  VALUES ($1, clock_timestamp() + make_interval(secs => $2))
  ON CONFLICT (ip) DO UPDATE SET banned_until = GREATEST(ip_bans.banned_until, EXCLUDED.banned_until)
 `, ip, ipBanDuration.Seconds())
		cancel()
		if err != nil {
			log.Printf("ip ban write failed: ip=%s: %v", ip, err)
		}
	}
}
