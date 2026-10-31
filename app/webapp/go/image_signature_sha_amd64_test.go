//go:build goexperiment.simd && amd64.v2

package isureal

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"simd/archsimd"
	"testing"

	"example.com/isureal-company/isureal/internal/sha256ni"
)

func TestSHA256NIIterations(t *testing.T) {
	if !archsimd.X86.SHA() {
		t.Skip("SHA instructions unavailable")
	}
	key := sha256.Sum256([]byte("SHA-NI differential"))
	base := newHMACLane(key[:], "画像", -36)
	for _, rounds := range []int{0, 1, 2, 16, 999, 1000} {
		lane := base
		sha256ni.Iterate(&lane.istate, &lane.ostate, &lane.value, &sha256K, rounds)
		if lane.istate != base.istate || lane.ostate != base.ostate {
			t.Fatalf("modified initial HMAC state after %d rounds", rounds)
		}
		mac := hmac.New(sha256.New, key[:])
		want := base.bytes()
		for range rounds {
			mac.Reset()
			mac.Write(want)
			want = mac.Sum(want[:0])
		}
		if got := lane.bytes(); !bytes.Equal(got, want) {
			t.Fatalf("%d rounds: got %x, want %x", rounds, got, want)
		}
	}
}
