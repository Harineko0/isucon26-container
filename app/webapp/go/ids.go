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
