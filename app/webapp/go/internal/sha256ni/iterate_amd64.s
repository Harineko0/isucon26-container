//go:build goexperiment.simd && amd64.v2

#include "textflag.h"

// Convert A..H in two vectors to the ABEF / CDGH packing used by SHA-NI.
#define LOAD_STATE(ptr, abef, cdgh) \
	MOVOU (ptr), abef; \
	MOVOU 16(ptr), cdgh; \
	PSHUFD $0xb1, abef, abef; \
	PSHUFD $0x1b, cdgh, cdgh; \
	MOVO abef, X7; \
	PALIGNR $8, cdgh, abef; \
	PBLENDW $0xf0, X7, cdgh

// X3..X6 form a rolling schedule of sixteen words. Once four rounds have
// consumed w0, replace it with the next four words, t+16 through t+19.
#define NEXT4(w0, w1, w2, w3) \
	SHA256MSG1 w1, w0; \
	MOVO w3, X7; \
	PALIGNR $4, w2, X7; \
	PADDD X7, w0; \
	SHA256MSG2 w3, w0

// X0 supplies the two W+K operands to SHA256RNDS2. Use the low pair first.
#define ROUND4(w, offset) \
	MOVO w, X0; \
	MOVOU offset(AX), X7; \
	PADDD X7, X0; \
	SHA256RNDS2 X0, X1, X2; \
	PSHUFD $0x0e, X0, X0; \
	SHA256RNDS2 X0, X2, X1

// All messages after the initial HMAC consist of eight digest words, the
// padding bit, six zero words and (64 + 32) * 8 bits. The previous digest is
// already in X3:X4 and ends up there again, so no byte swap or spill is needed.
#define COMPRESS(abef, cdgh) \
	MOVO abef, X1; \
	MOVO cdgh, X2; \
	MOVO X12, X5; \
	MOVO X13, X6; \
	ROUND4(X3, 0);   NEXT4(X3, X4, X5, X6); \
	ROUND4(X4, 16);  NEXT4(X4, X5, X6, X3); \
	ROUND4(X5, 32);  NEXT4(X5, X6, X3, X4); \
	ROUND4(X6, 48);  NEXT4(X6, X3, X4, X5); \
	ROUND4(X3, 64);  NEXT4(X3, X4, X5, X6); \
	ROUND4(X4, 80);  NEXT4(X4, X5, X6, X3); \
	ROUND4(X5, 96);  NEXT4(X5, X6, X3, X4); \
	ROUND4(X6, 112); NEXT4(X6, X3, X4, X5); \
	ROUND4(X3, 128); NEXT4(X3, X4, X5, X6); \
	ROUND4(X4, 144); NEXT4(X4, X5, X6, X3); \
	ROUND4(X5, 160); NEXT4(X5, X6, X3, X4); \
	ROUND4(X6, 176); NEXT4(X6, X3, X4, X5); \
	ROUND4(X3, 192); \
	ROUND4(X4, 208); \
	ROUND4(X5, 224); \
	ROUND4(X6, 240); \
	PADDD abef, X1; \
	PADDD cdgh, X2; \
	PSHUFD $0x1b, X1, X3; \
	PSHUFD $0xb1, X2, X4; \
	MOVO X3, X7; \
	PBLENDW $0xf0, X4, X3; \
	PALIGNR $8, X7, X4

TEXT ·Iterate(SB), NOSPLIT, $0-40
	MOVQ n+32(FP), CX
	TESTQ CX, CX
	JLE done
	MOVQ inner+0(FP), SI
	LOAD_STATE(SI, X8, X9)
	MOVQ outer+8(FP), SI
	LOAD_STATE(SI, X10, X11)
	MOVQ constants+24(FP), AX
	MOVQ digest+16(FP), DI
	MOVOU (DI), X3
	MOVOU 16(DI), X4
	MOVL $0x80000000, DX
	MOVD DX, X12
	MOVL $768, DX
	MOVD DX, X13
	PSLLDQ $12, X13

loop:
	COMPRESS(X8, X9)
	COMPRESS(X10, X11)
	DECQ CX
	JNZ loop

	MOVOU X3, (DI)
	MOVOU X4, 16(DI)
done:
	RET
