// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import _ "unsafe" // for go:linkname

//go:linkname dstHostRoutableIPString net.dstHostRoutableIPString
func dstHostRoutableIPString(host uint32) string

// HostIP returns host name's deterministic routable IP (dotted-decimal string)
// within the simulation, so a process can address a peer host without DNS (which is
// unsupported under DST):
//
//	c, err := net.Dial("tcp", simulation.HostIP("n2")+":8080")
//
// Under the per-host network, loopback (127.0.0.1) is host-private; a process
// reaches another host only by that host's routable IP. A host listening on a
// wildcard or its own routable IP is reachable here from any host. Must be called
// inside a simulation; it panics on a host name no declaration has established. A string
// is returned rather than a net.IP so this package does not import net.
func HostIP(name string) string {
	return dstHostRoutableIPString(lookupHost(name))
}
