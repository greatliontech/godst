// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// DST in-memory pipe fixtures. os-only imports — this file must stay
// cgo-free (testprog hosts the runtime deadlock crash tests; a cgo-pulling
// import disables deadlock detection — see design.md "Enforcing test
// configurations").

package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"testing/simulation"
	"time"
)

func init() {
	register("DSTPipeReplay", DSTPipeReplay)
}

// DSTPipeReplay prints a transcript of a concurrent pipe workload under one
// seed: three producers write framed records through one os.Pipe while a
// consumer drains it in 64-byte sips, then the stream, the sip-size vector
// (a function of how bytes happened to be available per read; the
// schedule-sensitivity teeth ride the frame ORDER in the content line), the
// drain's terminal error and byte total (the completeness pin: 477 bytes is
// schedule-invariant — every record arrives exactly once on every schedule),
// the pipe's Stat shape, and a fake-clock deadline event are printed. The
// parent test runs this binary twice and requires byte-identical output —
// the cross-process replay form of the pipe determinism invariant (the
// in-process form lives in os's dst tests).
func DSTPipeReplay() {
	seed := dstSeedEnv()
	simulation.Run(seed, func() {
		r, w, err := os.Pipe()
		if err != nil {
			fmt.Println("pipe:", err)
			return
		}
		var wg sync.WaitGroup
		for g := 0; g < 3; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 6; i++ {
					pad := strings.Repeat(string(rune('a'+g)), 5+(g*7+i*3)%40)
					if _, err := fmt.Fprintf(w, "[g%d:%d:%s]", g, i, pad); err != nil {
						fmt.Println("write:", err)
						return
					}
				}
			}(g)
		}
		go func() {
			wg.Wait()
			w.Close()
		}()
		var content []byte
		var sips []int
		var end error
		buf := make([]byte, 64)
		for {
			n, rerr := r.Read(buf)
			content = append(content, buf[:n]...)
			if n > 0 {
				sips = append(sips, n)
			}
			if rerr != nil {
				end = rerr
				break
			}
		}
		fmt.Printf("content=%s\n", content)
		fmt.Printf("sips=%v\n", sips)
		fmt.Printf("end=%v total=%d\n", end, len(content))
		fi, err := r.Stat()
		if err != nil {
			fmt.Println("stat:", err)
			return
		}
		fmt.Printf("stat=%v size=%d\n", fi.Mode(), fi.Size())
		r.Close()

		// A deadline event: fires at the exact virtual instant.
		r2, w2, err := os.Pipe()
		if err != nil {
			fmt.Println("pipe2:", err)
			return
		}
		start := time.Now()
		r2.SetReadDeadline(start.Add(3 * time.Second))
		_, derr := r2.Read(make([]byte, 1))
		fmt.Printf("deadline: +%v err=%v\n", time.Since(start), derr)
		r2.Close()
		w2.Close()
	})
}
