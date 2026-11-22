#include "textflag.h"
#include "go_asm.h"

// MediaCodec async callbacks. DI is the codec, SI the *mcShared user data. They
// run on the codec's looper thread and never enter the Go runtime.

#define WAKE \
	LEAQ	mcShared_seq(SI), DI \
	INCL	(DI) \
	MOVQ	$129, SI /* FUTEX_WAKE_PRIVATE */ \
	MOVQ	$1, DX \
	MOVQ	$202, AX /* __NR_futex */ \
	SYSCALL \
	RET

// void input(codec, shared, int32 index)
TEXT ·mcInputThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVQ	mcShared_inTail(SI), AX
	MOVQ	AX, CX
	ANDQ	$const_mcRingMask, CX
	MOVL	DX, mcShared_inputs(SI)(CX*4)
	INCQ	AX
	MOVQ	AX, mcShared_inTail(SI)
	WAKE

// void output(codec, shared, int32 index, AMediaCodecBufferInfo *info)
TEXT ·mcOutputThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVQ	mcShared_outTail(SI), AX
	MOVQ	AX, R8
	ANDQ	$const_mcRingMask, R8
	SHLQ	$5, R8
	LEAQ	mcShared_outputs(SI)(R8*1), R9
	MOVL	DX, 0(R9)
	MOVQ	0(CX), R10
	MOVQ	R10, 8(R9)
	MOVQ	8(CX), R10
	MOVQ	R10, 16(R9)
	MOVQ	16(CX), R10
	MOVQ	R10, 24(R9)
	INCQ	AX
	MOVQ	AX, mcShared_outTail(SI)
	WAKE

// void format(codec, shared, AMediaFormat *format)
TEXT ·mcFormatThunk(SB), NOSPLIT|NOFRAME, $0-0
	INCL	mcShared_formats(SI)
	WAKE

// void error(codec, shared, media_status_t error, int32 action, char *detail)
TEXT ·mcErrorThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVL	DX, mcShared_status(SI)
	WAKE

// func mcThunks() (input, output, format, failure uintptr)
TEXT ·mcThunks(SB), NOSPLIT, $0-32
	LEAQ	·mcInputThunk(SB), AX
	MOVQ	AX, input+0(FP)
	LEAQ	·mcOutputThunk(SB), AX
	MOVQ	AX, output+8(FP)
	LEAQ	·mcFormatThunk(SB), AX
	MOVQ	AX, format+16(FP)
	LEAQ	·mcErrorThunk(SB), AX
	MOVQ	AX, failure+24(FP)
	RET
