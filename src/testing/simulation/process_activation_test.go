// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"os"
	"strconv"
	"syscall"
	"testing"
)

func TestDSTProcessPIDLivenessPublishedWithinAdmission(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "node.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var process *ast.FuncDecl
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "Process" {
			process = fn
			break
		}
	}
	if process == nil {
		t.Fatal("Process declaration not found")
	}
	countCalls := func(node ast.Node, name string) int {
		count := 0
		ast.Inspect(node, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == name {
				count++
			}
			return true
		})
		return count
	}
	total := countCalls(process.Body, "dstSetPidLive")
	inside := 0
	ast.Inspect(process.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		if !ok || id.Name != "withBubbleDeclCaller" || len(call.Args) != 2 {
			return true
		}
		if callback, ok := call.Args[1].(*ast.FuncLit); ok {
			inside += countCalls(callback.Body, "dstSetPidLive")
		}
		return false
	})
	if total != 2 || inside != 1 {
		t.Fatalf("Process dstSetPidLive calls = total %d, inside admission %d; want 2/1", total, inside)
	}
}

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
	foreignPIDs := activeProcPIDs(lookupProc("db"))
	if len(foreignPIDs) != 1 {
		t.Fatalf("pre-run Process pids = %v, want one", foreignPIDs)
	}
	foreignPID := foreignPIDs[0]

	var killErr, staleKillErr, procfsErr, writeErr, dialErr error
	var registrationOK bool
	RunWith(1, Options{PID: 1_000_000}, func() {
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
		staleKillErr = syscall.Kill(int(foreignPID), 0)
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
	if killErr != nil || staleKillErr != syscall.ESRCH || procfsErr != nil || !registrationOK || writeErr != nil || dialErr != nil {
		t.Fatalf("pre-run Process exit tore into active run: kill=%v staleKill=%v procfs=%v registered=%v write=%v dial=%v", killErr, staleKillErr, procfsErr, registrationOK, writeErr, dialErr)
	}
	select {
	case <-foreignDone:
	default:
		t.Fatal("pre-run Process did not finish")
	}
}
