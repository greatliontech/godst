// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix && (!dst || !linux)

package runtime

// dstMappingSigpanic is a no-op when the simulation is not built in: there are
// no simulated mappings, so no fault can belong to one.
func dstMappingSigpanic(gp *g) {}
