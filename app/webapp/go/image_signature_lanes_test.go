package isureal

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestImageSignatureLanes(t *testing.T) {
	const exp = 1815192000
	for _, tc := range []struct{ count, iterations int }{
		{0, 1}, {1, 0}, {15, 1}, {16, 2}, {17, 3}, {33, imageSignatureIterations - 1},
	} {
		t.Run(fmt.Sprintf("%d_lanes_%d_iterations", tc.count, tc.iterations), func(t *testing.T) {
			lanes := make([]hmacLane, tc.count)
			want := make([][]byte, tc.count)
			for i := range lanes {
				id := fmt.Sprintf("image-%d", i)
				key := sha256.Sum256([]byte(fmt.Sprintf("key-%d", i)))
				lanes[i] = newHMACLane(key[:], id, exp)
				mac := hmac.New(sha256.New, key[:])
				msg := []byte(fmt.Sprintf("%d:%s", exp, id))
				for range tc.iterations + 1 {
					mac.Reset()
					mac.Write(msg)
					msg = mac.Sum(nil)
				}
				want[i] = msg
			}
			iterateLanes(lanes, tc.iterations)
			for i := range lanes {
				if got := lanes[i].bytes(); !bytes.Equal(got, want[i]) {
					t.Fatalf("lane %d: got %x, want %x", i, got, want[i])
				}
			}
		})
	}
}
