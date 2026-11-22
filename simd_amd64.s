#include "textflag.h"

// func interleaveUV16(dst, u, v *byte, n int)
// Writes u[i], v[i] pairs. n is a multiple of 16. SSE2 is baseline on amd64.
TEXT ·interleaveUV16(SB), NOSPLIT, $0-32
	MOVQ	dst+0(FP), DI
	MOVQ	u+8(FP), SI
	MOVQ	v+16(FP), DX
	MOVQ	n+24(FP), CX
loop:
	TESTQ	CX, CX
	JEQ	done
	MOVOU	(SI), X0
	MOVOU	(DX), X1
	MOVO	X0, X2
	PUNPCKLBW	X1, X0
	PUNPCKHBW	X1, X2
	MOVOU	X0, (DI)
	MOVOU	X2, 16(DI)
	ADDQ	$16, SI
	ADDQ	$16, DX
	ADDQ	$32, DI
	SUBQ	$16, CX
	JMP	loop
done:
	RET

// func deinterleaveUV16(src, u, v *byte, n int)
// Splits n pairs into u and v. n is a multiple of 16.
TEXT ·deinterleaveUV16(SB), NOSPLIT, $0-32
	MOVQ	src+0(FP), SI
	MOVQ	u+8(FP), DI
	MOVQ	v+16(FP), DX
	MOVQ	n+24(FP), CX
	PCMPEQW	X5, X5
	PSRLW	$8, X5
loop:
	TESTQ	CX, CX
	JEQ	done
	MOVOU	(SI), X0
	MOVOU	16(SI), X1
	MOVO	X0, X2
	MOVO	X1, X3
	PAND	X5, X0
	PAND	X5, X1
	PSRLW	$8, X2
	PSRLW	$8, X3
	PACKUSWB	X1, X0
	PACKUSWB	X3, X2
	MOVOU	X0, (DI)
	MOVOU	X2, (DX)
	ADDQ	$32, SI
	ADDQ	$16, DI
	ADDQ	$16, DX
	SUBQ	$16, CX
	JMP	loop
done:
	RET
