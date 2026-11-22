#include "textflag.h"

DATA nv12Shuffle<>+0(SB)/8, $0x0E0C0A0806040200
DATA nv12Shuffle<>+8(SB)/8, $0x0F0D0B0907050301
GLOBL nv12Shuffle<>(SB), RODATA|NOPTR, $16

#define WORDS32(name, v) \
	DATA name<>+0(SB)/8, $v \
	DATA name<>+8(SB)/8, $v \
	DATA name<>+16(SB)/8, $v \
	DATA name<>+24(SB)/8, $v \
	GLOBL name<>(SB), RODATA|NOPTR, $32

WORDS32(sign16, 0x8000800080008000)
WORDS32(max255, 0x00FF00FF00FF00FF)
WORDS32(alphaHi, 0xFF00FF00FF00FF00)
WORDS32(lowByte, 0x00FF00FF00FF00FF)

// LOADCOEFS broadcasts the coefs structure at R9.
#define LOADCOEFS \
	VPBROADCASTW 0(R9), Y10 \
	VPBROADCASTW 2(R9), Y11 \
	VPBROADCASTW 4(R9), Y12 \
	VPBROADCASTW 6(R9), Y13 \
	VPBROADCASTW 8(R9), Y14 \
	VPBROADCASTW 10(R9), Y15 \
	VPBROADCASTW 12(R9), Y8 \
	VPXOR Y9, Y9, Y9

// PIXELS16 converts 16 pixels: luma from (SI), chroma doubled from dwords in
// Y1 (U) and Y2 (V), and writes 64 bytes to (DI).
#define PIXELS16 \
	VPMOVZXBW (SI), Y0 \
	VPSLLW $7, Y0, Y0 \
	VPSUBSW Y10, Y0, Y0 \
	VPMULHRSW Y11, Y0, Y0 \
	VPADDW Y8, Y0, Y0 \
	VPSLLD $16, Y1, Y3 \
	VPOR Y3, Y1, Y1 \
	VPSLLW $8, Y1, Y1 \
	VPXOR sign16<>(SB), Y1, Y1 \
	VPSLLD $16, Y2, Y3 \
	VPOR Y3, Y2, Y2 \
	VPSLLW $8, Y2, Y2 \
	VPXOR sign16<>(SB), Y2, Y2 \
	VPMULHRSW Y12, Y1, Y3 \
	VPADDSW Y0, Y3, Y3 \
	VPMULHRSW Y13, Y1, Y4 \
	VPSUBSW Y4, Y0, Y4 \
	VPMULHRSW Y14, Y2, Y5 \
	VPSUBSW Y5, Y4, Y4 \
	VPMULHRSW Y15, Y2, Y5 \
	VPADDSW Y0, Y5, Y5 \
	VPSRAW $6, Y3, Y3 \
	VPSRAW $6, Y4, Y4 \
	VPSRAW $6, Y5, Y5 \
	VPMAXSW Y9, Y3, Y3 \
	VPMAXSW Y9, Y4, Y4 \
	VPMAXSW Y9, Y5, Y5 \
	VPMINSW max255<>(SB), Y3, Y3 \
	VPMINSW max255<>(SB), Y4, Y4 \
	VPMINSW max255<>(SB), Y5, Y5 \
	VPSLLW $8, Y4, Y4 \
	VPOR Y4, Y3, Y3 \
	VPOR alphaHi<>(SB), Y5, Y5 \
	VPUNPCKLWD Y5, Y3, Y6 \
	VPUNPCKHWD Y5, Y3, Y7 \
	VPERM2I128 $0x20, Y7, Y6, Y0 \
	VPERM2I128 $0x31, Y7, Y6, Y1 \
	VMOVDQU Y0, (DI) \
	VMOVDQU Y1, 32(DI)

// func i420Kernel(dst, y, u, v *byte, n int, c *coefs)
TEXT ·i420Kernel(SB), NOSPLIT, $0-48
	MOVQ	dst+0(FP), DI
	MOVQ	y+8(FP), SI
	MOVQ	u+16(FP), BX
	MOVQ	v+24(FP), DX
	MOVQ	n+32(FP), CX
	MOVQ	c+40(FP), R9
	LOADCOEFS
i420loop:
	VPMOVZXBD (BX), Y1
	VPMOVZXBD (DX), Y2
	PIXELS16
	ADDQ	$16, SI
	ADDQ	$8, BX
	ADDQ	$8, DX
	ADDQ	$64, DI
	SUBQ	$16, CX
	JG	i420loop
	VZEROUPPER
	RET

// func nv12Kernel(dst, y, uv *byte, n int, c *coefs)
TEXT ·nv12Kernel(SB), NOSPLIT, $0-40
	MOVQ	dst+0(FP), DI
	MOVQ	y+8(FP), SI
	MOVQ	uv+16(FP), BX
	MOVQ	n+24(FP), CX
	MOVQ	c+32(FP), R9
	LOADCOEFS
nv12loop:
	VMOVDQU	(BX), X1
	VPSHUFB	nv12Shuffle<>(SB), X1, X1
	VPSRLDQ	$8, X1, X2
	VPMOVZXBD X1, Y1
	VPMOVZXBD X2, Y2
	PIXELS16
	ADDQ	$16, SI
	ADDQ	$16, BX
	ADDQ	$64, DI
	SUBQ	$16, CX
	JG	nv12loop
	VZEROUPPER
	RET

// func interleaveUV16(dst, u, v *byte, n int)
TEXT ·interleaveUV16(SB), NOSPLIT, $0-32
	MOVQ	dst+0(FP), DI
	MOVQ	u+8(FP), SI
	MOVQ	v+16(FP), DX
	MOVQ	n+24(FP), CX
ileave:
	MOVOU	(SI), X0
	MOVOU	(DX), X1
	MOVO	X0, X2
	PUNPCKLBW X1, X0
	PUNPCKHBW X1, X2
	MOVOU	X0, (DI)
	MOVOU	X2, 16(DI)
	ADDQ	$16, SI
	ADDQ	$16, DX
	ADDQ	$32, DI
	SUBQ	$16, CX
	JG	ileave
	RET

// func deinterleaveUV16(src, u, v *byte, n int)
TEXT ·deinterleaveUV16(SB), NOSPLIT, $0-32
	MOVQ	src+0(FP), SI
	MOVQ	u+8(FP), DI
	MOVQ	v+16(FP), DX
	MOVQ	n+24(FP), CX
	MOVOU	lowByte<>(SB), X4
dleave:
	MOVOU	(SI), X0
	MOVOU	16(SI), X1
	MOVO	X0, X2
	MOVO	X1, X3
	PAND	X4, X0
	PAND	X4, X1
	PACKUSWB X1, X0
	PSRLW	$8, X2
	PSRLW	$8, X3
	PACKUSWB X3, X2
	MOVOU	X0, (DI)
	MOVOU	X2, (DX)
	ADDQ	$32, SI
	ADDQ	$16, DI
	ADDQ	$16, DX
	SUBQ	$16, CX
	JG	dleave
	RET

// func cpuid(leaf, sub uint32) (eax, ebx, ecx, edx uint32)
TEXT ·cpuid(SB), NOSPLIT, $0-24
	MOVL	leaf+0(FP), AX
	MOVL	sub+4(FP), CX
	CPUID
	MOVL	AX, eax+8(FP)
	MOVL	BX, ebx+12(FP)
	MOVL	CX, ecx+16(FP)
	MOVL	DX, edx+20(FP)
	RET

// func xgetbv() (eax, edx uint32)
TEXT ·xgetbv(SB), NOSPLIT, $0-8
	XORL	CX, CX
	XGETBV
	MOVL	AX, eax+0(FP)
	MOVL	DX, edx+4(FP)
	RET
