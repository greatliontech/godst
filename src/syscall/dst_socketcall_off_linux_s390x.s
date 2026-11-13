// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst && linux && s390x

#include "textflag.h"
#include "funcdata.h"

#define SYS_SOCKETCALL 102	/* from zsysnum_linux_s390x.go */

// func socketcall(call int, a0, a1, a2, a3, a4, a5 uintptr) (n int, err int)
TEXT ·socketcall(SB),NOSPLIT,$0-72
	BL	runtime·entersyscall(SB)
	MOVD	$SYS_SOCKETCALL, R1	// syscall entry
	MOVD	call+0(FP), R2		// socket call number
	MOVD	$a0+8(FP), R3		// pointer to call arguments
	MOVD	$0, R4
	MOVD	$0, R5
	MOVD	$0, R6
	MOVD	$0, R7
	SYSCALL
	MOVD	$0xfffffffffffff001, R8
	CMPUBLT	R2, R8, oksock
	MOVD	$-1, n+56(FP)
	NEG	R2, R2
	MOVD	R2, err+64(FP)
	BL	runtime·exitsyscall(SB)
	RET
oksock:
	MOVD	R2, n+56(FP)
	MOVD	$0, err+64(FP)
	CALL	runtime·exitsyscall(SB)
	RET

// func rawsocketcall(call int, a0, a1, a2, a3, a4, a5 uintptr) (n int, err int)
TEXT ·rawsocketcall(SB),NOSPLIT,$0-72
	MOVD	$SYS_SOCKETCALL, R1	// syscall entry
	MOVD	call+0(FP), R2		// socket call number
	MOVD	$a0+8(FP), R3		// pointer to call arguments
	MOVD	$0, R4
	MOVD	$0, R5
	MOVD	$0, R6
	MOVD	$0, R7
	SYSCALL
	MOVD	$0xfffffffffffff001, R8
	CMPUBLT	R2, R8, oksock1
	MOVD	$-1, n+56(FP)
	NEG	R2, R2
	MOVD	R2, err+64(FP)
	RET
oksock1:
	MOVD	R2, n+56(FP)
	MOVD	$0, err+64(FP)
	RET
