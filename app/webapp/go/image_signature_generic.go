//go:build !goexperiment.simd || !amd64.v2

package isureal

func computeImageSignature(key []byte, imageID string, exp int64) []byte {
	return computeImageSignatureGeneric(key, imageID, exp)
}
