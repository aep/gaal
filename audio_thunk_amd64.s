//go:build !android && !windows

#include "textflag.h"
#include "go_asm.h"

// Callbacks of libFLAC and a shim for liba52. They run on the C stack inside a
// library call and never enter the Go runtime. System V ABI: DI, SI, DX, CX, R8, R9.

// int write(encoder, buffer, size_t bytes, unsigned samples, unsigned frame, flacShared*)
TEXT ·flacEncWriteThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVQ	R9, R10
	MOVL	CX, R8                      // samples
	MOVQ	flacShared_outLen(R10), AX
	LEAQ	(AX)(DX*1), R11
	CMPQ	R11, flacShared_outCap(R10)
	JHI	over
	MOVQ	flacShared_out(R10), DI
	ADDQ	AX, DI
	MOVQ	DX, CX
	REP; MOVSB
	MOVQ	R11, flacShared_outLen(R10)
	TESTL	R8, R8
	JEQ	done
	MOVL	flacShared_count(R10), AX
	CMPL	AX, $const_flacMaxEnds
	JCC	more
	LEAQ	flacShared_ends(R10), CX
	MOVL	R11, (CX)(AX*4)
more:
	INCL	flacShared_count(R10)
done:
	XORL	AX, AX
	RET
over:
	MOVL	$1, flacShared_overflow(R10)
	XORL	AX, AX
	RET

// int read(decoder, buffer, size_t *bytes, flacShared*)
TEXT ·flacDecReadThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVQ	CX, R10
	MOVQ	(DX), R8
	MOVQ	flacShared_inLeft(R10), AX
	CMPQ	R8, AX
	CMOVQHI	AX, R8
	MOVQ	R8, (DX)
	TESTQ	R8, R8
	JEQ	end
	MOVQ	SI, DI
	MOVQ	flacShared_in(R10), SI
	MOVQ	R8, CX
	REP; MOVSB
	MOVQ	SI, flacShared_in(R10)
	SUBQ	R8, flacShared_inLeft(R10)
	XORL	AX, AX
	RET
end:
	MOVL	$1, AX // end of stream
	RET

// int write(decoder, FLAC__Frame*, const int32 *const buffer[], flacShared*)
// Stores the frame as interleaved 16-bit samples, whatever its depth.
TEXT ·flacDecWriteThunk(SB), NOSPLIT|NOFRAME, $0-0
	PUSHQ	BX
	PUSHQ	R12
	PUSHQ	R13
	MOVQ	CX, R10
	MOVL	0(SI), R8                   // blocksize
	MOVL	8(SI), R9                   // channels
	MOVL	4(SI), AX
	MOVL	AX, flacShared_rate(R10)
	MOVL	R9, flacShared_channels(R10)
	MOVL	16(SI), BX                  // bits per sample
	MOVQ	R8, AX
	IMULQ	R9, AX
	MOVQ	flacShared_pcmLen(R10), R11
	LEAQ	(R11)(AX*1), R12
	CMPQ	R12, flacShared_pcmCap(R10)
	JHI	full
	MOVQ	R12, flacShared_pcmLen(R10)
	ADDL	R8, flacShared_blocks(R10)
	INCL	flacShared_decoded(R10)
	MOVQ	flacShared_pcm(R10), R12
	LEAQ	(R12)(R11*2), R11           // first sample of this frame
	MOVL	$32, CX
	SUBL	BX, CX                      // left-justify, then keep the top 16 bits
	LEAQ	(R9*2), R13                 // distance between samples of one channel
	XORL	BX, BX                      // channel
chan:
	MOVQ	(DX)(BX*8), SI
	LEAQ	(R11)(BX*2), DI
	MOVL	R8, R12
	TESTL	R12, R12
	JEQ	next
loop:
	MOVL	(SI), AX
	SHLL	CX, AX
	SARL	$16, AX
	MOVW	AX, (DI)
	ADDQ	$4, SI
	ADDQ	R13, DI
	DECL	R12
	JNE	loop
next:
	INCL	BX
	CMPL	BX, R9
	JCS	chan
	XORL	AX, AX
	JMP	out
full:
	MOVL	$1, flacShared_full(R10)
	XORL	AX, AX
out:
	POPQ	R13
	POPQ	R12
	POPQ	BX
	RET

// void error(decoder, status, flacShared*)
TEXT ·flacDecErrorThunk(SB), NOSPLIT|NOFRAME, $0-0
	INCL	flacShared_errors(DX)
	RET

// int shim(state, buf, flags*, level*, a52_frame): calls a52_frame with a bias of
// 0.0, a float argument that purego.SyscallN cannot set.
TEXT ·a52FrameThunk(SB), NOSPLIT|NOFRAME, $0-0
	XORPS	X0, X0
	JMP	R8

// func audioThunks() (encWrite, decRead, decWrite, decError, a52Frame uintptr)
TEXT ·audioThunks(SB), NOSPLIT, $0-40
	LEAQ	·flacEncWriteThunk(SB), AX
	MOVQ	AX, encWrite+0(FP)
	LEAQ	·flacDecReadThunk(SB), AX
	MOVQ	AX, decRead+8(FP)
	LEAQ	·flacDecWriteThunk(SB), AX
	MOVQ	AX, decWrite+16(FP)
	LEAQ	·flacDecErrorThunk(SB), AX
	MOVQ	AX, decError+24(FP)
	LEAQ	·a52FrameThunk(SB), AX
	MOVQ	AX, a52Frame+32(FP)
	RET
