// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"os"
	"testing"
)

// The simulated-file hot-path benchmarks: every gated os operation on a
// simulated file resolves its backend through the out-of-line state
// table (os/dst_filestate.go), so these arms price that lookup on the
// operation shapes that pay it most often — the regression net for any
// future per-structure residency judgment (design.md, "Untagged
// footprint (contract)"). The b.N loop runs inside one simulation:
// per-run setup amortizes as b.N grows, so the converged ns/op is the
// per-operation cost.

func BenchmarkSimFileOpenWriteClose(b *testing.B) {
	Run(1, func() {
		Host("h", HostConfig{}, func() {
			buf := []byte("x")
			for i := 0; i < b.N; i++ {
				f, err := os.Create("/bench")
				if err != nil {
					panic(err)
				}
				if _, err := f.Write(buf); err != nil {
					panic(err)
				}
				if err := f.Close(); err != nil {
					panic(err)
				}
			}
		})
	})
}

func BenchmarkSimFileReadAt(b *testing.B) {
	Run(1, func() {
		Host("h", HostConfig{}, func() {
			if err := os.WriteFile("/bench", []byte("payload"), 0o644); err != nil {
				panic(err)
			}
			f, err := os.Open("/bench")
			if err != nil {
				panic(err)
			}
			defer f.Close()
			buf := make([]byte, 4)
			for i := 0; i < b.N; i++ {
				if _, err := f.ReadAt(buf, 0); err != nil {
					panic(err)
				}
			}
		})
	})
}

func BenchmarkSimFileStat(b *testing.B) {
	Run(1, func() {
		Host("h", HostConfig{}, func() {
			if err := os.WriteFile("/bench", []byte("payload"), 0o644); err != nil {
				panic(err)
			}
			f, err := os.Open("/bench")
			if err != nil {
				panic(err)
			}
			defer f.Close()
			for i := 0; i < b.N; i++ {
				if _, err := f.Stat(); err != nil {
					panic(err)
				}
			}
		})
	})
}
