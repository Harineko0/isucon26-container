//go:build goexperiment.simd && amd64

#include "textflag.h"

// Each ZMM register holds the same word of sixteen independent SHA-256
// compressions. Z0..Z15 form the message schedule ring, Z16..Z23 hold a..h.
// Rotate register names between rounds instead of moving their contents.
#define ROUND(i, w, a, b, c, d, e, f, g, h) \
	VPBROADCASTD (4*i)(AX), Z27; \
	VPADDD w, h, h; \
	VPADDD Z27, h, h; \
	VPRORD $6, e, Z24; \
	VPRORD $11, e, Z25; \
	VPRORD $25, e, Z26; \
	VPTERNLOGD $0x96, Z26, Z25, Z24; \
	VMOVDQA32 e, Z25; \
	VPTERNLOGD $0xca, g, f, Z25; \
	VPADDD Z25, Z24, Z24; \
	VPADDD Z24, h, h; \
	VPADDD h, d, d; \
	VPRORD $2, a, Z24; \
	VPRORD $13, a, Z25; \
	VPRORD $22, a, Z26; \
	VPTERNLOGD $0x96, Z26, Z25, Z24; \
	VMOVDQA32 a, Z25; \
	VPTERNLOGD $0xe8, c, b, Z25; \
	VPADDD Z25, Z24, Z24; \
	VPADDD Z24, h, h

// w[t] = w[t-16] + sigma0(w[t-15]) + w[t-7] + sigma1(w[t-2]).
#define EXPAND(w0, w1, w9, w14) \
	VPRORD $7, w1, Z24; \
	VPRORD $18, w1, Z25; \
	VPSRLD $3, w1, Z26; \
	VPTERNLOGD $0x96, Z26, Z25, Z24; \
	VPADDD w9, w0, w0; \
	VPADDD Z24, w0, w0; \
	VPRORD $17, w14, Z24; \
	VPRORD $19, w14, Z25; \
	VPSRLD $10, w14, Z26; \
	VPTERNLOGD $0x96, Z26, Z25, Z24; \
	VPADDD Z24, w0, w0

// Rounds 0..15 already have their message words. Rounds 16..63 expand
// the next word in place before using it. AX points to the current 16 K's.
#define INPUT(w0, w1, w9, w14)
#define ROUNDS16(schedule) \
	schedule(Z0, Z1, Z9, Z14);   ROUND(0,  Z0,  Z16,Z17,Z18,Z19,Z20,Z21,Z22,Z23); \
	schedule(Z1, Z2, Z10, Z15);  ROUND(1,  Z1,  Z23,Z16,Z17,Z18,Z19,Z20,Z21,Z22); \
	schedule(Z2, Z3, Z11, Z0);   ROUND(2,  Z2,  Z22,Z23,Z16,Z17,Z18,Z19,Z20,Z21); \
	schedule(Z3, Z4, Z12, Z1);   ROUND(3,  Z3,  Z21,Z22,Z23,Z16,Z17,Z18,Z19,Z20); \
	schedule(Z4, Z5, Z13, Z2);   ROUND(4,  Z4,  Z20,Z21,Z22,Z23,Z16,Z17,Z18,Z19); \
	schedule(Z5, Z6, Z14, Z3);   ROUND(5,  Z5,  Z19,Z20,Z21,Z22,Z23,Z16,Z17,Z18); \
	schedule(Z6, Z7, Z15, Z4);   ROUND(6,  Z6,  Z18,Z19,Z20,Z21,Z22,Z23,Z16,Z17); \
	schedule(Z7, Z8, Z0, Z5);    ROUND(7,  Z7,  Z17,Z18,Z19,Z20,Z21,Z22,Z23,Z16); \
	schedule(Z8, Z9, Z1, Z6);    ROUND(8,  Z8,  Z16,Z17,Z18,Z19,Z20,Z21,Z22,Z23); \
	schedule(Z9, Z10, Z2, Z7);   ROUND(9,  Z9,  Z23,Z16,Z17,Z18,Z19,Z20,Z21,Z22); \
	schedule(Z10, Z11, Z3, Z8);  ROUND(10, Z10, Z22,Z23,Z16,Z17,Z18,Z19,Z20,Z21); \
	schedule(Z11, Z12, Z4, Z9);  ROUND(11, Z11, Z21,Z22,Z23,Z16,Z17,Z18,Z19,Z20); \
	schedule(Z12, Z13, Z5, Z10); ROUND(12, Z12, Z20,Z21,Z22,Z23,Z16,Z17,Z18,Z19); \
	schedule(Z13, Z14, Z6, Z11); ROUND(13, Z13, Z19,Z20,Z21,Z22,Z23,Z16,Z17,Z18); \
	schedule(Z14, Z15, Z7, Z12); ROUND(14, Z14, Z18,Z19,Z20,Z21,Z22,Z23,Z16,Z17); \
	schedule(Z15, Z0, Z8, Z13);  ROUND(15, Z15, Z17,Z18,Z19,Z20,Z21,Z22,Z23,Z16)

TEXT ·Iterate(SB), NOSPLIT, $0-40
	MOVQ n+32(FP), CX
	TESTQ CX, CX
	JLE done
	ADDQ CX, CX // one inner and one outer compression per iteration
	MOVQ inner+0(FP), R8
	MOVQ outer+8(FP), R9
	MOVQ digest+16(FP), DI
	MOVQ k+24(FP), R10
	VMOVDQU32   0(DI), Z0
	VMOVDQU32  64(DI), Z1
	VMOVDQU32 128(DI), Z2
	VMOVDQU32 192(DI), Z3
	VMOVDQU32 256(DI), Z4
	VMOVDQU32 320(DI), Z5
	VMOVDQU32 384(DI), Z6
	VMOVDQU32 448(DI), Z7
	MOVL $0x80000000, DX
	VPBROADCASTD DX, Z30
	MOVL $768, DX // (64-byte key pad + 32-byte digest) * 8
	VPBROADCASTD DX, Z31

compress:
	VMOVDQA32 Z30, Z8
	VPXORD Z9, Z9, Z9
	VPXORD Z10, Z10, Z10
	VPXORD Z11, Z11, Z11
	VPXORD Z12, Z12, Z12
	VPXORD Z13, Z13, Z13
	VPXORD Z14, Z14, Z14
	VMOVDQA32 Z31, Z15
	VMOVDQU32   0(R8), Z16
	VMOVDQU32  64(R8), Z17
	VMOVDQU32 128(R8), Z18
	VMOVDQU32 192(R8), Z19
	VMOVDQU32 256(R8), Z20
	VMOVDQU32 320(R8), Z21
	VMOVDQU32 384(R8), Z22
	VMOVDQU32 448(R8), Z23
	MOVQ R10, AX
	ROUNDS16(INPUT)
	MOVL $3, DX
next16:
	ADDQ $64, AX
	ROUNDS16(EXPAND)
	DECL DX
	JNZ next16

	// Feed forward directly into the input registers for the next digest.
	VPADDD   0(R8), Z16, Z0
	VPADDD  64(R8), Z17, Z1
	VPADDD 128(R8), Z18, Z2
	VPADDD 192(R8), Z19, Z3
	VPADDD 256(R8), Z20, Z4
	VPADDD 320(R8), Z21, Z5
	VPADDD 384(R8), Z22, Z6
	VPADDD 448(R8), Z23, Z7
	XCHGQ R8, R9
	DECQ CX
	JNZ compress
	VMOVDQU32 Z0,   0(DI)
	VMOVDQU32 Z1,  64(DI)
	VMOVDQU32 Z2, 128(DI)
	VMOVDQU32 Z3, 192(DI)
	VMOVDQU32 Z4, 256(DI)
	VMOVDQU32 Z5, 320(DI)
	VMOVDQU32 Z6, 384(DI)
	VMOVDQU32 Z7, 448(DI)
	VZEROUPPER
done:
	RET
