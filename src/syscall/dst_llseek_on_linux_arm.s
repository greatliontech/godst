// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && arm

#include "textflag.h"
#include "funcdata.h"

#define SYS__LLSEEK 140  /* from zsysnum_linux_arm.go */

// func rawSeek(fd int, offset int64, whence int) (newoffset int64, errno int)
TEXT ·rawSeek(SB),NOSPLIT,$0-28
	BL	runtime·entersyscall(SB)
	MOVW	$SYS__LLSEEK, R7	// syscall entry
	MOVW	fd+0(FP), R0
	MOVW	offset_hi+8(FP), R1
	MOVW	offset_lo+4(FP), R2
	MOVW	$newoffset_lo+16(FP), R3
	MOVW	whence+12(FP), R4
	SWI	$0
	MOVW	$0xfffff001, R6
	CMP	R6, R0
	BLS	okseek
	MOVW	$0, R1
	MOVW	R1, newoffset_lo+16(FP)
	MOVW	R1, newoffset_hi+20(FP)
	RSB	$0, R0, R0
	MOVW	R0, err+24(FP)
	BL	runtime·exitsyscall(SB)
	RET
okseek:
	// system call filled in newoffset already
	MOVW	$0, R0
	MOVW	R0, err+24(FP)
	BL	runtime·exitsyscall(SB)
	RET
