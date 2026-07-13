// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"math"
	"runtime"
	"strconv"
	"testing"
)

func TestDSTHostNumCPUExactRange(t *testing.T) {
	for _, want := range numCPURangeValues() {
		t.Run(strconv.FormatInt(int64(want), 10), func(t *testing.T) {
			var direct, child int
			Run(1, func() {
				Host("h", HostConfig{NumCPU: want}, func() {
					direct = runtime.NumCPU()
					done := make(chan struct{})
					go func() {
						child = runtime.NumCPU()
						close(done)
					}()
					<-done
				})
			})
			if direct != want || child != want {
				t.Fatalf("runtime.NumCPU = direct %d, child %d; want %d", direct, child, want)
			}
		})
	}
}

func TestDSTOptionsNumCPUExactRange(t *testing.T) {
	for _, want := range numCPURangeValues() {
		t.Run(strconv.FormatInt(int64(want), 10), func(t *testing.T) {
			var got int
			RunWith(1, Options{NumCPU: want}, func() { got = runtime.NumCPU() })
			if got != want {
				t.Fatalf("runtime.NumCPU = %d, want %d", got, want)
			}
		})
	}
}

func TestDSTHostNumCPULargeRedeclarationExact(t *testing.T) {
	if strconv.IntSize < 64 {
		t.Skip("large positive int requires a 64-bit target")
	}
	max32 := int64(math.MaxInt32)
	wrapsPositive := int64(1)<<32 + 7
	first := int(max32 + 1)
	second := int(wrapsPositive)
	var gotFirst, gotSecond int
	Run(1, func() {
		Host("h", HostConfig{NumCPU: first}, func() { gotFirst = runtime.NumCPU() })
		Host("h", HostConfig{NumCPU: second}, func() { gotSecond = runtime.NumCPU() })
	})
	if gotFirst != first || gotSecond != second {
		t.Fatalf("runtime.NumCPU across redeclaration = %d then %d, want %d then %d", gotFirst, gotSecond, first, second)
	}
}

func numCPURangeValues() []int {
	values := []int{math.MaxInt32}
	if strconv.IntSize == 64 {
		max32 := int64(math.MaxInt32)
		wrapsPositive := int64(1)<<32 + 7
		values = append(values, int(max32+1), int(wrapsPositive), int(^uint(0)>>1))
	}
	return values
}
