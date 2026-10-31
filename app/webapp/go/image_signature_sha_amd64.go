//go:build goexperiment.simd && amd64.v2

package isureal

import (
	"crypto/sha256"
	"simd/archsimd"

	"example.com/isureal-company/isureal/internal/sha256ni"
)

func computeImageSignature(key []byte, imageID string, exp int64) []byte {
	if !archsimd.X86.SHA() || len(key) != sha256.Size {
		return computeImageSignatureGeneric(key, imageID, exp)
	}
	lane := newHMACLane(key, imageID, exp)
	sha256ni.Iterate(&lane.istate, &lane.ostate, &lane.value, &sha256K, imageSignatureIterations-1)
	return lane.bytes()
}
