package isureal

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// These are independently generated HMAC-SHA256 vectors over
// exp + ":" + id for the first round, then 32 digest bytes for 999 rounds.
func TestImageSignatureDispatch(t *testing.T) {
	for _, tc := range []struct {
		key  []byte
		id   string
		exp  int64
		want string
	}{
		{bytes.Repeat([]byte{0xad}, 32), "01828cdf-c37f-4162-8768-4388c1ae2b9e", 1815192000, "09056d059df9129a88244a7dd8abe90490fdf6d2750ec10d5529689cae3aa47b"},
		{nil, "", 0, "b6540c7d2829d8c5d7e270fcb6b788e902dd0e8b3d700b3c43d8311ccdfb0550"},
		{bytes.Repeat([]byte{13}, 31), "画像:%25", -1741, "b66d0939a6acc7bab7d55d5419e81e2ce0025bf6af3465ae1548eedf80e9b1d0"},
		{bytes.Repeat([]byte{0xff}, 65), strings.Repeat("x", 97), 1<<63 - 1, "93af38b328a1185a7ca877f42ce6abff5aa9c5c368886a27d498b6bb0e771007"},
		{make([]byte, 32), strings.Repeat("r", 128), -1 << 63, "447af2f7f7a471b2f8860001d721f9060587e9ed3fb0f0e537683f277bfdfcc2"},
	} {
		got := computeImageSignature(tc.key, tc.id, tc.exp)
		if hex.EncodeToString(got) != tc.want {
			t.Errorf("keyLen=%d idLen=%d exp=%d: got %x, want %s", len(tc.key), len(tc.id), tc.exp, got, tc.want)
		}
	}
}

func BenchmarkImageSignatureDispatch(b *testing.B) {
	key := bytes.Repeat([]byte{0xad}, 32)
	const id = "01828cdf-c37f-4162-8768-4388c1ae2b9e"
	const exp = 1815192000
	for _, tc := range []struct {
		name string
		sign func([]byte, string, int64) []byte
	}{
		{"generic", computeImageSignatureGeneric}, {"dispatch", computeImageSignature},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				tc.sign(key, id, exp)
			}
		})
	}
}
