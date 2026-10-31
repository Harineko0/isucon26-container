//go:build goexperiment.simd && amd64

package isureal

import "simd/archsimd"

// AVX-512 があり SHA 命令がない CPU (asia1) では、16 本の署名を 1 命令でまとめて反復する。
// SHA 命令がある CPU では標準ライブラリの方が速いので使わない。
var useSIMDSignatures = archsimd.X86.AVX512() && !archsimd.X86.SHA()

type vec16 = archsimd.Uint32x16

var sha256KVec [64]vec16

func init() {
	if !useSIMDSignatures {
		return
	}
	for i, k := range sha256K {
		sha256KVec[i] = archsimd.BroadcastUint32x16(k)
	}
}

func sha256Compress16(state [8]vec16, w [16]vec16) [8]vec16 {
	a, b, c, d, e, f, g, h := state[0], state[1], state[2], state[3], state[4], state[5], state[6], state[7]
	for i := 0; i < 64; i++ {
		if i >= 16 {
			w15, w2 := w[(i-15)&15], w[(i-2)&15]
			s0 := w15.RotateAllRight(7).Xor(w15.RotateAllRight(18)).Xor(w15.ShiftAllRight(3))
			s1 := w2.RotateAllRight(17).Xor(w2.RotateAllRight(19)).Xor(w2.ShiftAllRight(10))
			w[i&15] = w[i&15].Add(s0).Add(w[(i-7)&15]).Add(s1)
		}
		sigma1 := e.RotateAllRight(6).Xor(e.RotateAllRight(11)).Xor(e.RotateAllRight(25))
		ch := e.And(f).Xor(g.AndNot(e))
		t1 := h.Add(sigma1).Add(ch).Add(sha256KVec[i]).Add(w[i&15])
		sigma0 := a.RotateAllRight(2).Xor(a.RotateAllRight(13)).Xor(a.RotateAllRight(22))
		maj := a.And(b).Xor(a.And(c)).Xor(b.And(c))
		t2 := sigma0.Add(maj)
		h, g, f, e, d, c, b, a = g, f, e, d.Add(t1), c, b, a, t1.Add(t2)
	}
	return [8]vec16{
		state[0].Add(a), state[1].Add(b), state[2].Add(c), state[3].Add(d),
		state[4].Add(e), state[5].Add(f), state[6].Add(g), state[7].Add(h),
	}
}

func iterateLanes(lanes []hmacLane, n int) {
	if !useSIMDSignatures {
		for i := range lanes {
			lanes[i].iterate(n)
		}
		return
	}
	pad := archsimd.BroadcastUint32x16(0x80000000)
	length := archsimd.BroadcastUint32x16((64 + 32) * 8)
	var zero vec16
	for base := 0; base < len(lanes); base += 16 {
		var istate, ostate, cur [8]vec16
		for j := 0; j < 8; j++ {
			var is, os, cv [16]uint32
			for l := 0; l < 16; l++ {
				lane := &lanes[base]
				if base+l < len(lanes) {
					lane = &lanes[base+l]
				}
				is[l], os[l], cv[l] = lane.istate[j], lane.ostate[j], lane.value[j]
			}
			istate[j], ostate[j], cur[j] = archsimd.LoadUint32x16(&is), archsimd.LoadUint32x16(&os), archsimd.LoadUint32x16(&cv)
		}
		for range n {
			block := [16]vec16{cur[0], cur[1], cur[2], cur[3], cur[4], cur[5], cur[6], cur[7], pad, zero, zero, zero, zero, zero, zero, length}
			inner := sha256Compress16(istate, block)
			block = [16]vec16{inner[0], inner[1], inner[2], inner[3], inner[4], inner[5], inner[6], inner[7], pad, zero, zero, zero, zero, zero, zero, length}
			cur = sha256Compress16(ostate, block)
		}
		for j := 0; j < 8; j++ {
			var out [16]uint32
			cur[j].Store(&out)
			for l := 0; l < 16 && base+l < len(lanes); l++ {
				lanes[base+l].value[j] = out[l]
			}
		}
	}
}
