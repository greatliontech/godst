// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux && amd64

#include "textflag.h"
#include "funcdata.h"

#define SYS_gettimeofday 96

// func rawGettimeofday(tv *Timeval) (err uintptr)
// The fenced Go wrapper gettimeofday (dst_gettimeofday_on_linux_amd64.go) is the named entry.
TEXT ·rawGettimeofday(SB),NOSPLIT,$0-16
	// Usually, we'd check if we're running
	// secret code here, but because we execute
	// gettimeofday on the G stack, it's fine to leave
	// the registers uncleared
	MOVQ	tv+0(FP), DI
	MOVQ	$0, SI
	MOVQ	runtime·vdsoGettimeofdaySym(SB), AX
	TESTQ   AX, AX
	JZ fallback
	CALL	AX
ret:
	CMPQ	AX, $0xfffffffffffff001
	JLS	ok7
	NEGQ	AX
	MOVQ	AX, err+8(FP)
	RET
fallback:
	MOVL	$SYS_gettimeofday, AX
	SYSCALL
	JMP ret
ok7:
	MOVQ	$0, err+8(FP)
	RET
