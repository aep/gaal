//go:build !android && !windows

#include "textflag.h"
#include "go_asm.h"

// Callbacks of libFLAC and a shim for liba52. They run on the C stack inside a
// library call and never enter the Go runtime. R18 is left alone: it is reserved
// on Apple platforms.

// int write(encoder, buffer, size_t bytes, unsigned samples, unsigned frame, flacShared*)
TEXT ·flacEncWriteThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVD	flacShared_outLen(R5), R6
	ADD	R2, R6, R7
	MOVD	flacShared_outCap(R5), R8
	CMP	R8, R7
	BHI	over
	MOVD	flacShared_out(R5), R8
	ADD	R6, R8, R8
	CBZ	R2, copied
copy:
	MOVBU.P	1(R1), R9
	MOVB.P	R9, 1(R8)
	SUB	$1, R2, R2
	CBNZ	R2, copy
copied:
	MOVD	R7, flacShared_outLen(R5)
	CBZW	R3, done
	MOVWU	flacShared_count(R5), R9
	CMPW	$const_flacMaxEnds, R9
	BHS	more
	ADD	$flacShared_ends, R5, R10
	MOVW	R7, (R10)(R9<<2)
more:
	ADD	$1, R9, R9
	MOVW	R9, flacShared_count(R5)
done:
	MOVD	ZR, R0
	RET	(R30)
over:
	MOVD	$1, R9
	MOVW	R9, flacShared_overflow(R5)
	MOVD	ZR, R0
	RET	(R30)

// int read(decoder, buffer, size_t *bytes, flacShared*)
TEXT ·flacDecReadThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVD	(R2), R4
	MOVD	flacShared_inLeft(R3), R5
	CMP	R5, R4
	CSEL	HI, R5, R4, R4
	MOVD	R4, (R2)
	CBZ	R4, end
	MOVD	flacShared_in(R3), R6
	SUB	R4, R5, R5
	MOVD	R5, flacShared_inLeft(R3)
rcopy:
	MOVBU.P	1(R6), R7
	MOVB.P	R7, 1(R1)
	SUB	$1, R4, R4
	CBNZ	R4, rcopy
	MOVD	R6, flacShared_in(R3)
	MOVD	ZR, R0
	RET	(R30)
end:
	MOVD	$1, R0 // end of stream
	RET	(R30)

// int write(decoder, FLAC__Frame*, const int32 *const buffer[], flacShared*)
// Stores the frame as interleaved 16-bit samples, whatever its depth.
TEXT ·flacDecWriteThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVWU	0(R1), R4                   // blocksize
	MOVWU	8(R1), R5                   // channels
	MOVWU	4(R1), R6
	MOVW	R6, flacShared_rate(R3)
	MOVW	R5, flacShared_channels(R3)
	MOVWU	16(R1), R6                  // bits per sample
	MUL	R4, R5, R7
	MOVD	flacShared_pcmLen(R3), R8
	ADD	R7, R8, R9
	MOVD	flacShared_pcmCap(R3), R10
	CMP	R10, R9
	BHI	full
	MOVD	R9, flacShared_pcmLen(R3)
	MOVWU	flacShared_blocks(R3), R9
	ADD	R4, R9, R9
	MOVW	R9, flacShared_blocks(R3)
	MOVWU	flacShared_decoded(R3), R9
	ADD	$1, R9, R9
	MOVW	R9, flacShared_decoded(R3)
	MOVD	flacShared_pcm(R3), R9
	ADD	R8<<1, R9, R9               // first sample of this frame
	MOVD	$32, R10
	SUB	R6, R10, R10                // left-justify, then keep the top 16 bits
	LSL	$1, R5, R11                 // distance between samples of one channel
	MOVD	ZR, R12                     // channel
chan:
	MOVD	(R2)(R12<<3), R13
	ADD	R12<<1, R9, R14
	MOVD	R4, R15
	CBZ	R15, next
loop:
	MOVWU.P	4(R13), R16
	LSLW	R10, R16, R16
	ASRW	$16, R16, R16
	MOVH	R16, (R14)
	ADD	R11, R14, R14
	SUB	$1, R15, R15
	CBNZ	R15, loop
next:
	ADD	$1, R12, R12
	CMP	R5, R12
	BLO	chan
	MOVD	ZR, R0
	RET	(R30)
full:
	MOVD	$1, R9
	MOVW	R9, flacShared_full(R3)
	MOVD	ZR, R0
	RET	(R30)

// void error(decoder, status, flacShared*)
TEXT ·flacDecErrorThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVWU	flacShared_errors(R2), R3
	ADD	$1, R3, R3
	MOVW	R3, flacShared_errors(R2)
	RET	(R30)

// int shim(state, buf, flags*, level*, a52_frame): calls a52_frame with a bias of
// 0.0, a float argument that purego.SyscallN cannot set.
TEXT ·a52FrameThunk(SB), NOSPLIT|NOFRAME, $0-0
	FMOVS	ZR, F0
	JMP	(R4)

// func audioThunks() (encWrite, decRead, decWrite, decError, a52Frame uintptr)
TEXT ·audioThunks(SB), NOSPLIT, $0-40
	MOVD	$·flacEncWriteThunk(SB), R0
	MOVD	R0, encWrite+0(FP)
	MOVD	$·flacDecReadThunk(SB), R0
	MOVD	R0, decRead+8(FP)
	MOVD	$·flacDecWriteThunk(SB), R0
	MOVD	R0, decWrite+16(FP)
	MOVD	$·flacDecErrorThunk(SB), R0
	MOVD	R0, decError+24(FP)
	MOVD	$·a52FrameThunk(SB), R0
	MOVD	R0, a52Frame+32(FP)
	RET
