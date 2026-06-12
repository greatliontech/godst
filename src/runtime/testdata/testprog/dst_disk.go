// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// DST in-memory filesystem fixtures. os-only imports — this file must stay
// cgo-free (testprog hosts the runtime deadlock crash tests; a cgo-pulling
// import disables deadlock detection — see design.md "Enforcing test
// configurations").

package main

import (
	"fmt"
	"io"
	"os"
	"sync"
	"testing/simulation"
)

func init() {
	register("DSTDiskReplay", DSTDiskReplay)
}

// DSTDiskReplay prints a transcript of a concurrent file workload under one
// seed: four goroutines append tagged records to one O_APPEND file while a
// fifth re-reads it at every step, then the final content and size are
// printed. The parent test runs this binary twice and requires byte-identical
// output — the cross-process replay form of the filesystem determinism
// invariant (the in-process form lives in os's dst tests).
func DSTDiskReplay() {
	seed := dstSeedEnv()
	simulation.Run(seed, func() {
		f, err := os.OpenFile("/replay.log", os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Println("open:", err)
			return
		}
		var mu sync.Mutex
		var sizes []int64
		var wg sync.WaitGroup
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := 0; i < 8; i++ {
					fmt.Fprintf(f, "[g%d:%d]", g, i)
					if fi, err := f.Stat(); err == nil {
						mu.Lock()
						sizes = append(sizes, fi.Size())
						mu.Unlock()
					}
				}
			}(g)
		}
		wg.Wait()
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			fmt.Println("seek:", err)
			return
		}
		all, err := io.ReadAll(f)
		if err != nil {
			fmt.Println("read:", err)
			return
		}
		f.Close()
		fmt.Printf("content=%s\n", all)
		fmt.Printf("sizes=%v\n", sizes)
	})
}
