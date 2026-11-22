#include "textflag.h"

// func interleaveUV16(dst, u, v *byte, n int)
// Writes u[i], v[i] pairs. n is a multiple of 16.
TEXT ·interleaveUV16(SB), NOSPLIT, $0-32
	MOVD	dst+0(FP), R0
	MOVD	u+8(FP), R1
	MOVD	v+16(FP), R2
	MOVD	n+24(FP), R3
loop:
	CBZ	R3, done
	VLD1.P	16(R1), [V0.B16]
	VLD1.P	16(R2), [V1.B16]
	VST2.P	[V0.B16, V1.B16], 32(R0)
	SUB	$16, R3
	B	loop
done:
	RET

// func deinterleaveUV16(src, u, v *byte, n int)
// Splits n pairs into u and v. n is a multiple of 16.
TEXT ·deinterleaveUV16(SB), NOSPLIT, $0-32
	MOVD	src+0(FP), R0
	MOVD	u+8(FP), R1
	MOVD	v+16(FP), R2
	MOVD	n+24(FP), R3
loop:
	CBZ	R3, done
	VLD2.P	32(R0), [V0.B16, V1.B16]
	VST1.P	[V0.B16], 16(R1)
	VST1.P	[V1.B16], 16(R2)
	SUB	$16, R3
	B	loop
done:
	RET
