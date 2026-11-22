#include "textflag.h"
#include "go_asm.h"

// MediaCodec async callbacks. R0 is the codec, R1 the *mcShared user data. They
// run on the codec's looper thread and never enter the Go runtime.

#define WAKE \
	ADD	$mcShared_seq, R1, R0 \
	LDARW	(R0), R3 \
	ADDW	$1, R3, R3 \
	STLRW	R3, (R0) \
	MOVD	$129, R1 /* FUTEX_WAKE_PRIVATE */ \
	MOVD	$1, R2 \
	MOVD	$98, R8 /* __NR_futex */ \
	SVC \
	RET	(R30)

// void input(codec, shared, int32 index)
TEXT ·mcInputThunk(SB), NOSPLIT|NOFRAME, $0-0
	ADD	$mcShared_inTail, R1, R8
	LDAR	(R8), R3
	AND	$const_mcRingMask, R3, R4
	ADD	$mcShared_inputs, R1, R5
	MOVW	R2, (R5)(R4<<2)
	ADD	$1, R3, R3
	STLR	R3, (R8)
	WAKE

// void output(codec, shared, int32 index, AMediaCodecBufferInfo *info)
TEXT ·mcOutputThunk(SB), NOSPLIT|NOFRAME, $0-0
	ADD	$mcShared_outTail, R1, R8
	LDAR	(R8), R4
	AND	$const_mcRingMask, R4, R5
	LSL	$5, R5, R5
	ADD	$mcShared_outputs, R1, R6
	ADD	R5, R6, R6
	MOVW	R2, 0(R6)
	MOVD	0(R3), R7
	MOVD	R7, 8(R6)
	MOVD	8(R3), R7
	MOVD	R7, 16(R6)
	MOVD	16(R3), R7
	MOVD	R7, 24(R6)
	ADD	$1, R4, R4
	STLR	R4, (R8)
	WAKE

// void format(codec, shared, AMediaFormat *format)
TEXT ·mcFormatThunk(SB), NOSPLIT|NOFRAME, $0-0
	ADD	$mcShared_formats, R1, R8
	LDARW	(R8), R3
	ADDW	$1, R3, R3
	STLRW	R3, (R8)
	WAKE

// void error(codec, shared, media_status_t error, int32 action, char *detail)
TEXT ·mcErrorThunk(SB), NOSPLIT|NOFRAME, $0-0
	ADD	$mcShared_status, R1, R8
	STLRW	R2, (R8)
	WAKE

// func mcThunks() (input, output, format, failure uintptr)
TEXT ·mcThunks(SB), NOSPLIT, $0-32
	MOVD	$·mcInputThunk(SB), R0
	MOVD	R0, input+0(FP)
	MOVD	$·mcOutputThunk(SB), R0
	MOVD	R0, output+8(FP)
	MOVD	$·mcFormatThunk(SB), R0
	MOVD	R0, format+16(FP)
	MOVD	$·mcErrorThunk(SB), R0
	MOVD	R0, failure+24(FP)
	RET
