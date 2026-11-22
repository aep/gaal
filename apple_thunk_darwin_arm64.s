#include "textflag.h"
#include "go_asm.h"

// void output(vtShared *s, void *ref, OSStatus status, uint32 flags, CFTypeRef obj)
// Queues one VideoToolbox result. It may run on any thread.
TEXT ·vtOutputThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVD.W	R30, -48(RSP)
	MOVD	R0, 8(RSP)
	MOVD	R1, 16(RSP)
	MOVD	R2, 24(RSP)
	MOVD	R3, 32(RSP)
	MOVD	R4, 40(RSP)
	CBZ	R4, store
	MOVD	vtShared_retain(R0), R5
	MOVD	R4, R0
	CALL	(R5)
	MOVD	8(RSP), R0
	MOVD	16(RSP), R1
	MOVD	24(RSP), R2
	MOVD	32(RSP), R3
	MOVD	40(RSP), R4
store:
	ADD	$vtShared_tail, R0, R8
	LDAR	(R8), R6
	AND	$const_vtRingMask, R6, R7
	MOVD	$const_vtOutputSize, R9
	MUL	R9, R7, R7
	ADD	$vtShared_ring, R0, R9
	ADD	R7, R9, R9
	MOVW	R2, vtOutput_status(R9)
	MOVW	R3, vtOutput_flags(R9)
	MOVD	R1, vtOutput_ref(R9)
	MOVD	R4, vtOutput_obj(R9)
	ADD	$1, R6, R6
	STLR	R6, (R8)
	MOVD.P	48(RSP), R30
	RET	(R30)

// OSStatus input(conv, UInt32 *ioPackets, AudioBufferList *data,
//                AudioStreamPacketDescription **desc, auShared *s)
// Supplies the one pending packet, then reports that no more input is ready.
TEXT ·auInputThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVWU	auShared_avail(R4), R5
	CBZW	R5, none
	MOVWU	auShared_packets(R4), R5
	MOVW	R5, (R1)
	MOVD	$1, R5
	MOVW	R5, auBufferList_count(R2)
	MOVWU	auShared_channels(R4), R5
	MOVW	R5, auBufferList_channels(R2)
	MOVWU	auShared_bytes(R4), R5
	MOVW	R5, auBufferList_bytes(R2)
	MOVD	auShared_data(R4), R5
	MOVD	R5, auBufferList_data(R2)
	CBZ	R3, done
	ADD	$auShared_desc, R4, R5
	MOVD	R5, (R3)
done:
	MOVW	ZR, auShared_avail(R4)
	MOVD	ZR, R0
	RET	(R30)
none:
	MOVW	ZR, (R1)
	MOVD	$const_auNoData, R0
	RET	(R30)

// func vtThunks() (output, audio uintptr)
TEXT ·vtThunks(SB), NOSPLIT, $0-16
	MOVD	$·vtOutputThunk(SB), R0
	MOVD	R0, output+0(FP)
	MOVD	$·auInputThunk(SB), R0
	MOVD	R0, audio+8(FP)
	RET
