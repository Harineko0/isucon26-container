//go:build !(goexperiment.simd && amd64)

package isureal

const useSIMDSignatures = false

func iterateLanes(lanes []hmacLane, n int) {
	for i := range lanes {
		lanes[i].iterate(n)
	}
}
