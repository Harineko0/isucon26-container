//go:build goexperiment.simd && amd64

package isureal

import (
	"simd/archsimd"

	"example.com/isureal-company/isureal/internal/sha256x16"
)

// SHA-capable machines use the single-signature path.
var useSIMDSignatures = archsimd.X86.AVX512() && !archsimd.X86.SHA()

func iterateLanes(lanes []hmacLane, n int) {
	if !archsimd.X86.AVX512() {
		for i := range lanes {
			lanes[i].iterate(n)
		}
		return
	}
	for len(lanes) > 0 {
		batch := lanes[:min(16, len(lanes))]
		var inner, outer, digest sha256x16.Words
		for i := range batch {
			for w := range digest {
				inner[w][i] = batch[i].istate[w]
				outer[w][i] = batch[i].ostate[w]
				digest[w][i] = batch[i].value[w]
			}
		}
		sha256x16.Iterate(&inner, &outer, &digest, &sha256K, n)
		for i := range batch {
			for w := range digest {
				batch[i].value[w] = digest[w][i]
			}
		}
		lanes = lanes[len(batch):]
	}
}
