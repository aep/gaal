#include "textflag.h"

// The conversion kernels use instructions the Go assembler has no names for,
// so they are encoded as words. Each line shows the instruction it encodes.

// func i420Kernel(dst, y, u, v *byte, n int, c *coefs)
TEXT ·i420Kernel(SB), NOSPLIT, $0-48
	MOVD	dst+0(FP), R0
	MOVD	y+8(FP), R1
	MOVD	u+16(FP), R2
	MOVD	v+24(FP), R3
	MOVD	n+32(FP), R4
	MOVD	c+40(FP), R5
	WORD	$0x4c4074a0	// ld1 { v0.8h }, [x5]
	WORD	$0x4f07e7ff	// movi v31.16b, #0xff
	WORD	$0x4f04a41e	// movi v30.8h, #0x80, lsl #8
	WORD	$0x4e020410	// dup v16.8h, v0.h[0]
	WORD	$0x4e1a0411	// dup v17.8h, v0.h[6]
	WORD	$0x4cdf7021	// ld1 { v1.16b }, [x1], #16
	WORD	$0x0cdf7042	// ld1 { v2.8b }, [x2], #8
	WORD	$0x0cdf7063	// ld1 { v3.8b }, [x3], #8
	WORD	$0x4e023842	// zip1 v2.16b, v2.16b, v2.16b
	WORD	$0x4e033863	// zip1 v3.16b, v3.16b, v3.16b
	WORD	$0x2f0fa424	// ushll v4.8h, v1.8b, #0x7
	WORD	$0x6f0fa425	// ushll2 v5.8h, v1.16b, #0x7
	WORD	$0x4e702c84	// sqsub v4.8h, v4.8h, v16.8h
	WORD	$0x4e702ca5	// sqsub v5.8h, v5.8h, v16.8h
	WORD	$0x4f50d084	// sqrdmulh v4.8h, v4.8h, v0.h[1]
	WORD	$0x4f50d0a5	// sqrdmulh v5.8h, v5.8h, v0.h[1]
	WORD	$0x4e718484	// add v4.8h, v4.8h, v17.8h
	WORD	$0x4e7184a5	// add v5.8h, v5.8h, v17.8h
	WORD	$0x2e213846	// shll v6.8h, v2.8b, #8
	WORD	$0x6e213847	// shll2 v7.8h, v2.16b, #8
	WORD	$0x2e213872	// shll v18.8h, v3.8b, #8
	WORD	$0x6e213873	// shll2 v19.8h, v3.16b, #8
	WORD	$0x6e3e1cc6	// eor v6.16b, v6.16b, v30.16b
	WORD	$0x6e3e1ce7	// eor v7.16b, v7.16b, v30.16b
	WORD	$0x6e3e1e52	// eor v18.16b, v18.16b, v30.16b
	WORD	$0x6e3e1e73	// eor v19.16b, v19.16b, v30.16b
	WORD	$0x4f60d0d4	// sqrdmulh v20.8h, v6.8h, v0.h[2]
	WORD	$0x4f60d0f5	// sqrdmulh v21.8h, v7.8h, v0.h[2]
	WORD	$0x4f70d0d6	// sqrdmulh v22.8h, v6.8h, v0.h[3]
	WORD	$0x4f70d0f7	// sqrdmulh v23.8h, v7.8h, v0.h[3]
	WORD	$0x4f40da58	// sqrdmulh v24.8h, v18.8h, v0.h[4]
	WORD	$0x4f40da79	// sqrdmulh v25.8h, v19.8h, v0.h[4]
	WORD	$0x4f50da5a	// sqrdmulh v26.8h, v18.8h, v0.h[5]
	WORD	$0x4f50da7b	// sqrdmulh v27.8h, v19.8h, v0.h[5]
	WORD	$0x4e640e94	// sqadd v20.8h, v20.8h, v4.8h
	WORD	$0x4e650eb5	// sqadd v21.8h, v21.8h, v5.8h
	WORD	$0x4e640f5a	// sqadd v26.8h, v26.8h, v4.8h
	WORD	$0x4e650f7b	// sqadd v27.8h, v27.8h, v5.8h
	WORD	$0x4e762c9c	// sqsub v28.8h, v4.8h, v22.8h
	WORD	$0x4e772cbd	// sqsub v29.8h, v5.8h, v23.8h
	WORD	$0x4e782f9c	// sqsub v28.8h, v28.8h, v24.8h
	WORD	$0x4e792fbd	// sqsub v29.8h, v29.8h, v25.8h
	WORD	$0x2f0a8688	// sqshrun v8.8b, v20.8h, #0x6
	WORD	$0x6f0a86a8	// sqshrun2 v8.16b, v21.8h, #0x6
	WORD	$0x2f0a8789	// sqshrun v9.8b, v28.8h, #0x6
	WORD	$0x6f0a87a9	// sqshrun2 v9.16b, v29.8h, #0x6
	WORD	$0x2f0a874a	// sqshrun v10.8b, v26.8h, #0x6
	WORD	$0x6f0a876a	// sqshrun2 v10.16b, v27.8h, #0x6
	WORD	$0x4ebf1feb	// mov v11.16b, v31.16b
	WORD	$0x4c9f0008	// st4 { v8.16b, v9.16b, v10.16b, v11.16b }, [x0], #64
	WORD	$0xf1004084	// subs x4, x4, #0x10
	WORD	$0x54fffa4c	// b.gt 0x14 <i420+0x14>
	RET

// func nv12Kernel(dst, y, uv *byte, n int, c *coefs)
TEXT ·nv12Kernel(SB), NOSPLIT, $0-40
	MOVD	dst+0(FP), R0
	MOVD	y+8(FP), R1
	MOVD	uv+16(FP), R2
	MOVD	n+24(FP), R4
	MOVD	c+32(FP), R5
	WORD	$0x4c4074a0	// ld1 { v0.8h }, [x5]
	WORD	$0x4f07e7ff	// movi v31.16b, #0xff
	WORD	$0x4f04a41e	// movi v30.8h, #0x80, lsl #8
	WORD	$0x4e020410	// dup v16.8h, v0.h[0]
	WORD	$0x4e1a0411	// dup v17.8h, v0.h[6]
	WORD	$0x4cdf7021	// ld1 { v1.16b }, [x1], #16
	WORD	$0x0cdf8042	// ld2 { v2.8b, v3.8b }, [x2], #16
	WORD	$0x4e023842	// zip1 v2.16b, v2.16b, v2.16b
	WORD	$0x4e033863	// zip1 v3.16b, v3.16b, v3.16b
	WORD	$0x2f0fa424	// ushll v4.8h, v1.8b, #0x7
	WORD	$0x6f0fa425	// ushll2 v5.8h, v1.16b, #0x7
	WORD	$0x4e702c84	// sqsub v4.8h, v4.8h, v16.8h
	WORD	$0x4e702ca5	// sqsub v5.8h, v5.8h, v16.8h
	WORD	$0x4f50d084	// sqrdmulh v4.8h, v4.8h, v0.h[1]
	WORD	$0x4f50d0a5	// sqrdmulh v5.8h, v5.8h, v0.h[1]
	WORD	$0x4e718484	// add v4.8h, v4.8h, v17.8h
	WORD	$0x4e7184a5	// add v5.8h, v5.8h, v17.8h
	WORD	$0x2e213846	// shll v6.8h, v2.8b, #8
	WORD	$0x6e213847	// shll2 v7.8h, v2.16b, #8
	WORD	$0x2e213872	// shll v18.8h, v3.8b, #8
	WORD	$0x6e213873	// shll2 v19.8h, v3.16b, #8
	WORD	$0x6e3e1cc6	// eor v6.16b, v6.16b, v30.16b
	WORD	$0x6e3e1ce7	// eor v7.16b, v7.16b, v30.16b
	WORD	$0x6e3e1e52	// eor v18.16b, v18.16b, v30.16b
	WORD	$0x6e3e1e73	// eor v19.16b, v19.16b, v30.16b
	WORD	$0x4f60d0d4	// sqrdmulh v20.8h, v6.8h, v0.h[2]
	WORD	$0x4f60d0f5	// sqrdmulh v21.8h, v7.8h, v0.h[2]
	WORD	$0x4f70d0d6	// sqrdmulh v22.8h, v6.8h, v0.h[3]
	WORD	$0x4f70d0f7	// sqrdmulh v23.8h, v7.8h, v0.h[3]
	WORD	$0x4f40da58	// sqrdmulh v24.8h, v18.8h, v0.h[4]
	WORD	$0x4f40da79	// sqrdmulh v25.8h, v19.8h, v0.h[4]
	WORD	$0x4f50da5a	// sqrdmulh v26.8h, v18.8h, v0.h[5]
	WORD	$0x4f50da7b	// sqrdmulh v27.8h, v19.8h, v0.h[5]
	WORD	$0x4e640e94	// sqadd v20.8h, v20.8h, v4.8h
	WORD	$0x4e650eb5	// sqadd v21.8h, v21.8h, v5.8h
	WORD	$0x4e640f5a	// sqadd v26.8h, v26.8h, v4.8h
	WORD	$0x4e650f7b	// sqadd v27.8h, v27.8h, v5.8h
	WORD	$0x4e762c9c	// sqsub v28.8h, v4.8h, v22.8h
	WORD	$0x4e772cbd	// sqsub v29.8h, v5.8h, v23.8h
	WORD	$0x4e782f9c	// sqsub v28.8h, v28.8h, v24.8h
	WORD	$0x4e792fbd	// sqsub v29.8h, v29.8h, v25.8h
	WORD	$0x2f0a8688	// sqshrun v8.8b, v20.8h, #0x6
	WORD	$0x6f0a86a8	// sqshrun2 v8.16b, v21.8h, #0x6
	WORD	$0x2f0a8789	// sqshrun v9.8b, v28.8h, #0x6
	WORD	$0x6f0a87a9	// sqshrun2 v9.16b, v29.8h, #0x6
	WORD	$0x2f0a874a	// sqshrun v10.8b, v26.8h, #0x6
	WORD	$0x6f0a876a	// sqshrun2 v10.16b, v27.8h, #0x6
	WORD	$0x4ebf1feb	// mov v11.16b, v31.16b
	WORD	$0x4c9f0008	// st4 { v8.16b, v9.16b, v10.16b, v11.16b }, [x0], #64
	WORD	$0xf1004084	// subs x4, x4, #0x10
	WORD	$0x54fffa6c	// b.gt 0xe4 <nv12+0x14>
	RET

// func compressKernel(p *byte, n int, m, k uint16)
TEXT ·compressKernel(SB), NOSPLIT, $0-20
	MOVD	p+0(FP), R0
	MOVD	n+8(FP), R1
	MOVHU	m+16(FP), R2
	MOVHU	k+18(FP), R3
	WORD	$0x4e010c40	// dup v0.16b, w2
	WORD	$0x4e020c61	// dup v1.8h, w3
	WORD	$0x4f008426	// movi v6.8h, #0x1
	WORD	$0x4c407002	// ld1 { v2.16b }, [x0]
	WORD	$0x2e20c043	// umull v3.8h, v2.8b, v0.8b
	WORD	$0x6e20c044	// umull2 v4.8h, v2.16b, v0.16b
	WORD	$0x4e618463	// add v3.8h, v3.8h, v1.8h
	WORD	$0x4e618484	// add v4.8h, v4.8h, v1.8h
	WORD	$0x6f180465	// ushr v5.8h, v3.8h, #0x8
	WORD	$0x4e658463	// add v3.8h, v3.8h, v5.8h
	WORD	$0x4e668463	// add v3.8h, v3.8h, v6.8h
	WORD	$0x6f180485	// ushr v5.8h, v4.8h, #0x8
	WORD	$0x4e658484	// add v4.8h, v4.8h, v5.8h
	WORD	$0x4e668484	// add v4.8h, v4.8h, v6.8h
	WORD	$0x0f088462	// shrn v2.8b, v3.8h, #0x8
	WORD	$0x4f088482	// shrn2 v2.16b, v4.8h, #0x8
	WORD	$0x4c9f7002	// st1 { v2.16b }, [x0], #16
	WORD	$0xf1004021	// subs x1, x1, #0x10
	WORD	$0x54fffe2c	// b.gt 0x1a8 <compress+0xc>
	RET

// func interleaveUV16(dst, u, v *byte, n int)
TEXT ·interleaveUV16(SB), NOSPLIT, $0-32
	MOVD	dst+0(FP), R0
	MOVD	u+8(FP), R1
	MOVD	v+16(FP), R2
	MOVD	n+24(FP), R3
ileave:
	VLD1.P	16(R1), [V0.B16]
	VLD1.P	16(R2), [V1.B16]
	VST2.P	[V0.B16, V1.B16], 32(R0)
	SUBS	$16, R3
	BGT	ileave
	RET

// func deinterleaveUV16(src, u, v *byte, n int)
TEXT ·deinterleaveUV16(SB), NOSPLIT, $0-32
	MOVD	src+0(FP), R0
	MOVD	u+8(FP), R1
	MOVD	v+16(FP), R2
	MOVD	n+24(FP), R3
dleave:
	VLD2.P	32(R0), [V0.B16, V1.B16]
	VST1.P	[V0.B16], 16(R1)
	VST1.P	[V1.B16], 16(R2)
	SUBS	$16, R3
	BGT	dleave
	RET
