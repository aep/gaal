//go:build !android

#include "textflag.h"
#include "go_asm.h"

// C ABI: R0 is the *nvShared user pointer, R1 the callback argument.

// int seq(nvShared*, CUVIDEOFORMAT*): reuse the decoder if the size is
// unchanged, otherwise continue in the Go callback.
TEXT ·nvSeqThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVWU	const_cvFCW(R1), R2
	MOVWU	nvShared_codedW(R0), R3
	CMPW	R2, R3
	BNE	slow
	MOVWU	const_cvFCH(R1), R2
	MOVWU	nvShared_codedH(R0), R3
	CMPW	R2, R3
	BNE	slow
	MOVWU	nvShared_surfaces(R0), R2
	CBZW	R2, slow
	MOVD	R2, R0
	RET	(R30)
slow:
	MOVD	nvShared_goSeq(R0), R2
	JMP	(R2)

// int decode(nvShared*, CUVIDPICPARAMS*): notes the picture for low-delay
// output, then decodes it. Returns 1 on success.
TEXT ·nvDecodeThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVD	nvShared_picTail(R0), R2
	AND	$const_nvRingMask, R2, R3
	LSL	$4, R3, R3
	ADD	$nvShared_pics, R0, R4
	ADD	R3, R4, R4
	MOVWU	const_cvICurr(R1), R5
	MOVW	R5, nvPicture_index(R4)
	MOVWU	const_cvIField(R1), R5
	MOVW	R5, nvPicture_field(R4)
	MOVWU	const_cvISecond(R1), R5
	MOVW	R5, nvPicture_second(R4)
	ADD	$1, R2, R2
	MOVD	R2, nvShared_picTail(R0)
	MOVD.W	R30, -16(RSP)
	MOVD	nvShared_decodeFn(R0), R2
	MOVD	nvShared_dec(R0), R0
	CALL	(R2)
	MOVD.P	16(RSP), R30
	CMPW	$0, R0
	CSETW	EQ, R0
	RET	(R30)

// int display(nvShared*, CUVIDPARSERDISPINFO*): queues the picture for Go.
TEXT ·nvDisplayThunk(SB), NOSPLIT|NOFRAME, $0-0
	CBZ	R1, done
	MOVD	nvShared_tail(R0), R2
	AND	$const_nvRingMask, R2, R3
	LSL	$4, R3, R3
	ADD	$nvShared_ring, R0, R4
	ADD	R3, R4, R4
	MOVWU	const_cvDIdx(R1), R5
	MOVW	R5, nvDisplay_index(R4)
	MOVWU	const_cvDProg(R1), R5
	MOVW	R5, nvDisplay_progressive(R4)
	MOVD	const_cvDTS(R1), R5
	MOVD	R5, nvDisplay_pts(R4)
	ADD	$1, R2, R2
	MOVD	R2, nvShared_tail(R0)
done:
	MOVD	$1, R0
	RET	(R30)

// func nvThunks() (seq, decode, display uintptr)
TEXT ·nvThunks(SB), NOSPLIT, $0-24
	MOVD	$·nvSeqThunk(SB), R0
	MOVD	R0, seq+0(FP)
	MOVD	$·nvDecodeThunk(SB), R0
	MOVD	R0, decode+8(FP)
	MOVD	$·nvDisplayThunk(SB), R0
	MOVD	R0, display+16(FP)
	RET
