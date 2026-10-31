//go:build goexperiment.simd && amd64

// Package sha256x16 implements the fixed-length HMAC rounds used for image signatures.
package sha256x16

// Words holds eight SHA-256 words from each of sixteen independent messages.
type Words [8][16]uint32

// Iterate replaces each digest with HMAC(key, digest), n times. Inner and outer
// hold the SHA-256 states after the ipad and opad blocks. k holds the 64 SHA-256
// round constants. The caller must check for AVX-512 before calling.
//
//go:noescape
func Iterate(inner, outer, digest *Words, k *[64]uint32, n int)
