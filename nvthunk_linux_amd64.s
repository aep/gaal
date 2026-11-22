//go:build !android

#include "textflag.h"
#include "go_asm.h"

// C ABI: DI is the *nvShared user pointer, SI the callback argument.

// int seq(nvShared*, CUVIDEOFORMAT*): reuse the decoder if the size is
// unchanged, otherwise continue in the Go callback.
TEXT ·nvSeqThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVL	const_cvFCW(SI), AX
	CMPL	AX, nvShared_codedW(DI)
	JNE	slow
	MOVL	const_cvFCH(SI), AX
	CMPL	AX, nvShared_codedH(DI)
	JNE	slow
	MOVL	nvShared_surfaces(DI), AX
	TESTL	AX, AX
	JEQ	slow
	RET
slow:
	MOVQ	nvShared_goSeq(DI), AX
	JMP	AX

// int decode(nvShared*, CUVIDPICPARAMS*): notes the picture for low-delay
// output, then decodes it. Returns 1 on success.
TEXT ·nvDecodeThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVQ	nvShared_picTail(DI), AX
	MOVQ	AX, CX
	ANDQ	$const_nvRingMask, CX
	SHLQ	$4, CX
	LEAQ	nvShared_pics(DI)(CX*1), DX
	MOVL	const_cvICurr(SI), R8
	MOVL	R8, nvPicture_index(DX)
	MOVL	const_cvIField(SI), R8
	MOVL	R8, nvPicture_field(DX)
	MOVL	const_cvISecond(SI), R8
	MOVL	R8, nvPicture_second(DX)
	INCQ	AX
	MOVQ	AX, nvShared_picTail(DI)
	SUBQ	$8, SP
	MOVQ	nvShared_decodeFn(DI), AX
	MOVQ	nvShared_dec(DI), DI
	CALL	AX
	ADDQ	$8, SP
	TESTL	AX, AX
	SETEQ	AX
	MOVBLZX	AX, AX
	RET

// int display(nvShared*, CUVIDPARSERDISPINFO*): queues the picture for Go.
TEXT ·nvDisplayThunk(SB), NOSPLIT|NOFRAME, $0-0
	TESTQ	SI, SI
	JEQ	done
	MOVQ	nvShared_tail(DI), AX
	MOVQ	AX, CX
	ANDQ	$const_nvRingMask, CX
	SHLQ	$4, CX
	LEAQ	nvShared_ring(DI)(CX*1), DX
	MOVL	const_cvDIdx(SI), R8
	MOVL	R8, nvDisplay_index(DX)
	MOVL	const_cvDProg(SI), R8
	MOVL	R8, nvDisplay_progressive(DX)
	MOVQ	const_cvDTS(SI), R8
	MOVQ	R8, nvDisplay_pts(DX)
	INCQ	AX
	MOVQ	AX, nvShared_tail(DI)
done:
	MOVL	$1, AX
	RET

// func nvThunks() (seq, decode, display uintptr)
TEXT ·nvThunks(SB), NOSPLIT, $0-24
	LEAQ	·nvSeqThunk(SB), AX
	MOVQ	AX, seq+0(FP)
	LEAQ	·nvDecodeThunk(SB), AX
	MOVQ	AX, decode+8(FP)
	LEAQ	·nvDisplayThunk(SB), AX
	MOVQ	AX, display+16(FP)
	RET
