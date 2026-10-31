package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"strconv"
)

// 署名は「HMAC-SHA256 を 1000 回反復」で、asia1 (SHA 命令なし) では CPU の大半を占める。
// 2 回目以降の反復は入力が常に 32 バイトなので、SHA-256 の圧縮関数 2 回 (内側・外側) に
// 固定できる。これを複数の署名でまとめて SIMD で計算する (結果は computeImageSignature と同じ)。

type signatureRequest struct {
	key     []byte
	imageID string
	exp     int64
}

var sha256K = [64]uint32{
	0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
	0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
	0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
	0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
	0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
	0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
	0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
	0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
}

var sha256IV = [8]uint32{0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19}

func rotr32(x uint32, n uint) uint32 { return x>>n | x<<(32-n) }

// sha256Compress は 1 ブロック分の SHA-256 圧縮 (フィードフォワード込み) を行う。
func sha256Compress(state [8]uint32, block *[16]uint32) [8]uint32 {
	var w [64]uint32
	copy(w[:16], block[:])
	for i := 16; i < 64; i++ {
		s0 := rotr32(w[i-15], 7) ^ rotr32(w[i-15], 18) ^ w[i-15]>>3
		s1 := rotr32(w[i-2], 17) ^ rotr32(w[i-2], 19) ^ w[i-2]>>10
		w[i] = w[i-16] + s0 + w[i-7] + s1
	}
	a, b, c, d, e, f, g, h := state[0], state[1], state[2], state[3], state[4], state[5], state[6], state[7]
	for i := 0; i < 64; i++ {
		t1 := h + (rotr32(e, 6) ^ rotr32(e, 11) ^ rotr32(e, 25)) + (e&f ^ ^e&g) + sha256K[i] + w[i]
		t2 := (rotr32(a, 2) ^ rotr32(a, 13) ^ rotr32(a, 22)) + (a&b ^ a&c ^ b&c)
		h, g, f, e, d, c, b, a = g, f, e, d+t1, c, b, a, t1+t2
	}
	return [8]uint32{state[0] + a, state[1] + b, state[2] + c, state[3] + d, state[4] + e, state[5] + f, state[6] + g, state[7] + h}
}

// hmacLane は 1 つの署名の反復状態。istate/ostate は ipad/opad を吸収した後の SHA-256 状態。
type hmacLane struct {
	istate, ostate [8]uint32
	value          [8]uint32
}

func newHMACLane(key []byte, imageID string, exp int64) hmacLane {
	var lane hmacLane
	var ipad, opad [16]uint32
	var keyBlock [64]byte
	copy(keyBlock[:], key) // 鍵は 32 バイト (imageSigningKey で検査済み) なのでハッシュ不要
	for i := 0; i < 16; i++ {
		word := binary.BigEndian.Uint32(keyBlock[i*4:])
		ipad[i] = word ^ 0x36363636
		opad[i] = word ^ 0x5c5c5c5c
	}
	lane.istate = sha256Compress(sha256IV, &ipad)
	lane.ostate = sha256Compress(sha256IV, &opad)
	// 1 回目は入力長が可変なので通常の HMAC で計算する。
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(strconv.FormatInt(exp, 10) + ":" + imageID))
	first := mac.Sum(nil)
	for i := 0; i < 8; i++ {
		lane.value[i] = binary.BigEndian.Uint32(first[i*4:])
	}
	return lane
}

// iterate は 32 バイト入力の HMAC を n 回繰り返す (スカラー版)。
func (lane *hmacLane) iterate(n int) {
	var block [16]uint32
	block[8] = 0x80000000
	block[15] = (64 + 32) * 8
	for range n {
		copy(block[:8], lane.value[:])
		inner := sha256Compress(lane.istate, &block)
		copy(block[:8], inner[:])
		lane.value = sha256Compress(lane.ostate, &block)
	}
}

func (lane *hmacLane) bytes() []byte {
	out := make([]byte, sha256.Size)
	for i := 0; i < 8; i++ {
		binary.BigEndian.PutUint32(out[i*4:], lane.value[i])
	}
	return out
}

// computeImageSignatures は複数の署名をまとめて計算する。結果はキャッシュにも入る。
func computeImageSignatures(requests []signatureRequest) {
	pending := make([]signatureRequest, 0, len(requests))
	seen := make(map[signatureCacheKey]bool, len(requests))
	for _, r := range requests {
		k := signatureCacheKey{key: string(r.key), imageID: r.imageID, exp: r.exp}
		if seen[k] {
			continue
		}
		seen[k] = true
		if _, ok := imageSignatures.get(k); !ok {
			pending = append(pending, r)
		}
	}
	if len(pending) == 0 {
		return
	}
	if !useSIMDSignatures {
		// SHA 命令がある CPU では標準ライブラリで 1 本ずつ計算する方が速い。
		for _, r := range pending {
			imageSignatures.put(signatureCacheKey{key: string(r.key), imageID: r.imageID, exp: r.exp}, computeImageSignature(r.key, r.imageID, r.exp))
		}
		return
	}
	lanes := make([]hmacLane, len(pending))
	for i, r := range pending {
		lanes[i] = newHMACLane(r.key, r.imageID, r.exp)
	}
	iterateLanes(lanes, imageSignatureIterations-1)
	for i, r := range pending {
		imageSignatures.put(signatureCacheKey{key: string(r.key), imageID: r.imageID, exp: r.exp}, lanes[i].bytes())
	}
}
