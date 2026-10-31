//go:build goexperiment.simd && amd64.v2

// Package sha256ni implements fixed-length image-signature HMAC rounds on SHA-capable CPUs.
package sha256ni

// Iterate applies n HMAC rounds to digest. The states after hashing the ipad and
// opad, and the digest itself, are SHA-256 words in their usual numeric order.
// The caller must check for the SHA extension. The v2 build requires SSE4.1 and SSSE3.
//
//go:noescape
func Iterate(inner, outer, digest *[8]uint32, constants *[64]uint32, n int)
