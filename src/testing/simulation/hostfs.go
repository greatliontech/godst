// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"io/fs"
	_ "unsafe" // for go:linkname
)

//go:linkname dstHostFSFor os.dstHostFSFor
func dstHostFSFor(host uint32) fs.FS

// HostFS returns a read-only view of host name's simulated filesystem, for harness
// assertions on a node's persisted state from outside that node — "did all replicas
// converge?", "did this host commit before it crashed?" — without running as the
// node (idiom 2). A process reading its OWN disk uses ordinary os calls inside its
// Host/Process body, exactly as real recovery code does (idiom 1). The view is
// read-only by construction (it is an fs.FS), so it can never become a cross-host
// back-channel write. It must be called inside a simulation (it reflects the run's
// current tree for host name; a host with no filesystem activity reports the empty
// baseline plus /tmp).
func HostFS(name string) fs.FS {
	return dstHostFSFor(internHost(name))
}
