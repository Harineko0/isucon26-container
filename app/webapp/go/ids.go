package isureal

import (
	"encoding/binary"
	"time"

	"github.com/google/uuid"
)

// newTimeID は投稿・画像・コメント・RealMoji の新しい ID を返す。
//
// 先頭 48 bit (16 進 12 桁 = 8-4 の部分) がミリ秒の UNIX 時刻、残り 74 bit が乱数。
// UUID v7 と同じ並びだが、版のニブルは v4 のまま、形式も 8-4-4-4-12 の小文字 16 進で、
// 通知 ID (admin_posting_window_api.go の gen_random_uuid を使う式) と同じ形。
// uuid.NewString() (v4) だと B-tree の挿入先が毎回ランダムな葉ページになるので、
// 時刻を先頭に置いて挿入先を右端に寄せる (汚れるページと full-page write を減らす)。
func newTimeID() string {
	u := uuid.New() // 版 (u[6] の上位 4 bit = 4) と variant (u[8]) はこのまま残す
	ms := uint64(time.Now().UnixMilli())
	binary.BigEndian.PutUint16(u[0:2], uint16(ms>>32))
	binary.BigEndian.PutUint32(u[2:6], uint32(ms))
	return u.String()
}

// timeIDSince は newTimeID を使い始めた日。それ以前の id (初期データ) は gen_random_uuid の乱数で、
// 先頭 48 bit を時刻と読むと 0〜8900 年のどこかに散らばる。この日から今の 1 時間先までに入るものだけを
// 時刻先頭の id とみなす (乱数が偶然この範囲に入る確率は 1e-7 程度。外れても親表で引き直すだけ)。
var timeIDSince = time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)

// timeIDTime は newTimeID (newImageID も同じ先頭) が id に埋めたミリ秒時刻を取り出す。
// 形が違うか、時刻が timeIDSince より前・今の 1 時間より先なら ok=false。
func timeIDTime(id string) (time.Time, bool) {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return time.Time{}, false
	}
	var ms int64
	for i := 0; i < 13; i++ {
		if i == 8 {
			continue
		}
		c := id[i]
		var v byte
		switch {
		case '0' <= c && c <= '9':
			v = c - '0'
		case 'a' <= c && c <= 'f':
			v = c - 'a' + 10
		default:
			return time.Time{}, false
		}
		ms = ms<<4 | int64(v)
	}
	t := time.UnixMilli(ms)
	if t.Before(timeIDSince) || t.After(time.Now().Add(time.Hour)) {
		return time.Time{}, false
	}
	return t, true
}
