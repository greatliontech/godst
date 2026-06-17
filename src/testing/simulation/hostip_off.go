// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package simulation

// HostIP requires building with -tags dst (the simulated network), and is only
// meaningful inside a simulation. This stub keeps HostIP referenceable from test
// files that compile untagged and skip at runtime (Run panics without the tag, so
// HostIP is never reached untagged), mirroring Host/Process being no-ops there.
func HostIP(name string) string { return "" }
