// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"net"
	"os"
	"strconv"
	"syscall"
	"testing"
)

func TestDSTPreRunProcessExitCannotTearIntoActiveRun(t *testing.T) {
	foreignStarted := make(chan struct{})
	releaseForeign := make(chan struct{})
	foreignDone := make(chan struct{})
	go func() {
		Process("db", func() {
			close(foreignStarted)
			<-releaseForeign
		})
		close(foreignDone)
	}()
	<-foreignStarted

	var killErr, procfsErr, writeErr, dialErr error
	var registrationOK bool
	Run(1, func() {
		type state struct {
			pid  int
			file *os.File
			port string
		}
		ready := make(chan state, 1)
		finish := make(chan struct{})
		currentDone := make(chan struct{})
		go func() {
			Process("db", func() {
				f, _ := os.Create("/current-db")
				ln, _ := net.Listen("tcp", ":0")
				_, port, _ := net.SplitHostPort(ln.Addr().String())
				ready <- state{pid: os.Getpid(), file: f, port: port}
				<-finish
			})
			close(currentDone)
		}()
		current := <-ready
		close(releaseForeign)
		<-foreignDone

		killErr = syscall.Kill(current.pid, 0)
		_, procfsErr = os.ReadFile("/proc/" + strconv.Itoa(current.pid) + "/stat")
		pids := activeProcPIDs(lookupProc("db"))
		registrationOK = len(pids) == 1 && pids[0] == int32(current.pid)
		_, writeErr = current.file.Write([]byte("alive"))
		if c, err := net.Dial("tcp", net.JoinHostPort(HostIP("db"), current.port)); err == nil {
			c.Close()
		} else {
			dialErr = err
		}
		close(finish)
		<-currentDone
	})
	if killErr != nil || procfsErr != nil || !registrationOK || writeErr != nil || dialErr != nil {
		t.Fatalf("pre-run Process exit tore into active run: kill=%v procfs=%v registered=%v write=%v dial=%v", killErr, procfsErr, registrationOK, writeErr, dialErr)
	}
	select {
	case <-foreignDone:
	default:
		t.Fatal("pre-run Process did not finish")
	}
}
