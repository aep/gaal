#include "textflag.h"
#include "go_asm.h"

// OSStatus input(conv, UInt32 *ioPackets, AudioBufferList *data,
//                AudioStreamPacketDescription **desc, atShared *s)
// Supplies the pending input once. With none pending it reports end of stream
// after Flush, and otherwise that no data is ready.
TEXT ·atInputThunk(SB), NOSPLIT|NOFRAME, $0-0
	MOVWU	atShared_avail(R4), R5
	CBZW	R5, none
	MOVWU	atShared_packets(R4), R5
	MOVW	R5, (R1)
	MOVD	$1, R5
	MOVW	R5, auBufferList_count(R2)
	MOVWU	atShared_channels(R4), R5
	MOVW	R5, auBufferList_channels(R2)
	MOVWU	atShared_bytes(R4), R5
	MOVW	R5, auBufferList_bytes(R2)
	MOVD	atShared_data(R4), R5
	MOVD	R5, auBufferList_data(R2)
	CBZ	R3, supplied
	ADD	$atShared_desc, R4, R5
	MOVD	R5, (R3)
supplied:
	MOVW	ZR, atShared_avail(R4)
	MOVD	ZR, R0
	RET	(R30)
none:
	MOVW	ZR, (R1)
	MOVW	ZR, auBufferList_bytes(R2)
	MOVWU	atShared_eof(R4), R5
	CBNZW	R5, eof
	MOVD	$const_auNoData, R0
	RET	(R30)
eof:
	MOVD	ZR, R0
	RET	(R30)

// func atThunk() uintptr
TEXT ·atThunk(SB), NOSPLIT, $0-8
	MOVD	$·atInputThunk(SB), R0
	MOVD	R0, ret+0(FP)
	RET
