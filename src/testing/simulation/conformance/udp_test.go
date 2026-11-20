// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package conformance

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"syscall"
	"testing"
	"testing/simulation"
	"time"
)

const udpSO_REUSEPORT = 0xf

func slotUDP(w *world, slot int) *net.UDPConn {
	if slot < 0 || slot >= len(w.udps) {
		return nil
	}
	return w.udps[slot]
}

func udpReusePortControl(network, address string, c syscall.RawConn) error {
	var sockErr error
	if err := c.Control(func(fd uintptr) {
		sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, udpSO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	return sockErr
}

func udpListen(network, addr string, reusePort, track bool) op {
	return op{
		name: fmt.Sprintf("udp-listen(%s, %q, reuseport=%v) track=%v", network, addr, reusePort, track),
		run: func(w *world) outcome {
			var lc net.ListenConfig
			if reusePort {
				lc.Control = udpReusePortControl
			}
			pc, err := lc.ListenPacket(context.Background(), network, addr)
			var uc *net.UDPConn
			if pc != nil {
				uc = pc.(*net.UDPConn)
			}
			if track {
				w.udps = append(w.udps, uc)
			} else if uc != nil {
				uc.Close()
			}
			return outcome{Err: errClass(err), N: -1}
		},
	}
}

func udpListenSameAddr(baseSlot int, reusePort, track bool) op {
	return op{
		name: fmt.Sprintf("udp-listen-same(base %d, reuseport=%v) track=%v", baseSlot, reusePort, track),
		run: func(w *world) outcome {
			base := slotUDP(w, baseSlot)
			if base == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			var lc net.ListenConfig
			if reusePort {
				lc.Control = udpReusePortControl
			}
			pc, err := lc.ListenPacket(context.Background(), "udp4", base.LocalAddr().String())
			var uc *net.UDPConn
			if pc != nil {
				uc = pc.(*net.UDPConn)
			}
			if track {
				w.udps = append(w.udps, uc)
			} else if uc != nil {
				uc.Close()
			}
			return outcome{Err: errClass(err), N: -1}
		},
	}
}

func udpDialSlot(srvSlot int, track bool) op {
	return op{
		name: fmt.Sprintf("udp-dial(srv %d) track=%v", srvSlot, track),
		run: func(w *world) outcome {
			srv := slotUDP(w, srvSlot)
			if srv == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			ra := srv.LocalAddr().(*net.UDPAddr)
			c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ra.Port})
			if track {
				w.udps = append(w.udps, c)
			} else if c != nil {
				c.Close()
			}
			return outcome{Err: errClass(err), N: -1}
		},
	}
}

func udpSendToSlot(srcSlot, dstSlot int, payload []byte) op {
	return op{
		name:      fmt.Sprintf("udp-sendto(src %d -> dst %d, %d bytes)", srcSlot, dstSlot, len(payload)),
		writeSize: len(payload),
		run: func(w *world) outcome {
			src := slotUDP(w, srcSlot)
			dst := slotUDP(w, dstSlot)
			if src == nil || dst == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			ra := dst.LocalAddr().(*net.UDPAddr)
			target := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ra.Port}
			n, err := src.WriteToUDP(payload, target)
			return outcome{Err: errClass(err), N: n}
		},
	}
}

func udpConnWrite(slot int, payload []byte) op {
	return op{
		name:      fmt.Sprintf("udp-write(slot %d, %d bytes)", slot, len(payload)),
		writeSize: len(payload),
		run: func(w *world) outcome {
			u := slotUDP(w, slot)
			if u == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			n, err := u.Write(payload)
			return outcome{Err: errClass(err), N: n}
		},
	}
}

func udpReadMsg(slot, bufCap int, guard time.Duration) op {
	return op{
		name: fmt.Sprintf("udp-readmsg(slot %d, cap %d, guard %v)", slot, bufCap, guard),
		run: func(w *world) outcome {
			u := slotUDP(w, slot)
			if u == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			if guard > 0 {
				u.SetReadDeadline(time.Now().Add(guard))
				defer u.SetReadDeadline(time.Time{})
			}
			buf := make([]byte, bufCap)
			n, oobn, flags, _, err := u.ReadMsgUDP(buf, nil)
			if err != nil {
				return outcome{Err: errClass(err), N: n}
			}
			trunc := (flags & syscall.MSG_TRUNC) != 0
			return outcome{
				Err:   "",
				N:     n,
				State: fmt.Sprintf("oob=%d trunc=%v %s", oobn, trunc, contentHash(buf[:n])),
			}
		},
	}
}

func udpRead(slot, bufCap int, guard time.Duration) op {
	return op{
		name: fmt.Sprintf("udp-read(slot %d, cap %d, guard %v)", slot, bufCap, guard),
		run: func(w *world) outcome {
			u := slotUDP(w, slot)
			if u == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			if guard > 0 {
				u.SetReadDeadline(time.Now().Add(guard))
				defer u.SetReadDeadline(time.Time{})
			}
			buf := make([]byte, bufCap)
			n, err := u.Read(buf)
			if err != nil {
				return outcome{Err: errClass(err), N: n}
			}
			return outcome{Err: "", N: n, State: contentHash(buf[:n])}
		},
	}
}

func udpErrorProbe(kind string, slot int) op {
	return op{
		name: fmt.Sprintf("udp-err-probe(%s, slot %d)", kind, slot),
		run: func(w *world) outcome {
			u := slotUDP(w, slot)
			if u == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			switch kind {
			case "unconnected-write":
				n, err := u.Write([]byte("x"))
				return outcome{Err: errClass(err), N: n}
			case "connected-writeto":
				n, err := u.WriteToUDP([]byte("x"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9})
				return outcome{Err: errClass(err), N: n}
			case "sendto-port0":
				n, err := u.WriteToUDP([]byte("x"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
				return outcome{Err: errClass(err), N: n}
			case "oversize-v4":
				n, err := u.Write(make([]byte, 65508))
				return outcome{Err: errClass(err), N: n}
			case "set-buffers":
				if err := u.SetReadBuffer(65536); err != nil {
					return outcome{Err: errClass(err), N: -1}
				}
				err := u.SetWriteBuffer(65536)
				return outcome{Err: errClass(err), N: -1}
			default:
				panic("unknown udp probe: " + kind)
			}
		},
	}
}

func udpFileOp(slot int) op {
	return op{
		name: fmt.Sprintf("udp-file(slot %d)", slot),
		run: func(w *world) outcome {
			u := slotUDP(w, slot)
			if u == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			f, err := u.File()
			if err != nil {
				return outcome{Err: errClass(err), N: -1}
			}
			f.Close()
			return outcome{N: -1, State: "file=ok"}
		},
	}
}

func udpClose(slot int) op {
	return op{
		name: fmt.Sprintf("udp-close(slot %d)", slot),
		run: func(w *world) outcome {
			u := slotUDP(w, slot)
			if u == nil {
				return outcome{Err: nilSlot, N: -1}
			}
			return outcome{Err: errClass(u.Close()), N: -1}
		},
	}
}

func udpAllowlist() []allowEntry {
	return []allowEntry{
		{
			key:  "udp-file-fenced",
			cite: "docs/dst/design.md — Simulated UDP datagram network (File() duplication remains fenced)",
			match: func(o op, host, sim outcome) bool {
				return strings.HasPrefix(o.name, "udp-file(") &&
					host.Err == "" && host.State == "file=ok" &&
					sim.Err == "OpError(file)/dst-unsupported" && sim.State == ""
			},
		},
	}
}

func genUDPOps(seed uint64, steps int) []op {
	rng := rand.New(rand.NewPCG(seed, seed^0x5544505f434f4e46))
	var ops []op

	type sockModel struct {
		slot      int
		connected bool
		closed    bool
		reusePort bool
		queued    int
	}
	var socks []sockModel
	nextSlot := 0

	// Bootstrap one unconnected server, one SO_REUSEPORT group, and one connected client.
	ops = append(ops, udpListen("udp4", "127.0.0.1:0", false, true))
	socks = append(socks, sockModel{slot: nextSlot})
	srv0 := nextSlot
	nextSlot++

	ops = append(ops, udpListen("udp4", "127.0.0.1:0", true, true))
	socks = append(socks, sockModel{slot: nextSlot, reusePort: true})
	rpBase := nextSlot
	nextSlot++

	ops = append(ops, udpListenSameAddr(rpBase, true, true))
	socks = append(socks, sockModel{slot: nextSlot, reusePort: true})
	nextSlot++

	// Non-reuseport conflict against srv0 and against rpBase.
	ops = append(ops, udpListenSameAddr(srv0, false, false))
	ops = append(ops, udpListenSameAddr(rpBase, false, false))

	ops = append(ops, udpDialSlot(srv0, true))
	socks = append(socks, sockModel{slot: nextSlot, connected: true})
	cli0 := nextSlot
	nextSlot++

	// Exercise deterministic error probes and the File() fence.
	ops = append(ops,
		udpErrorProbe("unconnected-write", srv0),
		udpErrorProbe("connected-writeto", cli0),
		udpErrorProbe("sendto-port0", srv0),
		udpErrorProbe("oversize-v4", cli0),
		udpErrorProbe("set-buffers", srv0),
		udpFileOp(srv0),
	)

	// Zero-byte datagram + short-read MSG_TRUNC probe.
	ops = append(ops,
		udpConnWrite(cli0, nil),
		udpReadMsg(srv0, 16, guardReady),
		udpConnWrite(cli0, pat(24, 0x11)),
		udpReadMsg(srv0, 8, guardReady),
	)

	// Connected UDP to closed server port -> Write succeeds, next Read returns ECONNREFUSED,
	// subsequent Read times out (socket stays open!).
	ops = append(ops, udpListen("udp4", "127.0.0.1:0", false, true))
	ephemSrv := nextSlot
	socks = append(socks, sockModel{slot: ephemSrv})
	nextSlot++
	ops = append(ops, udpDialSlot(ephemSrv, true))
	refCli := nextSlot
	socks = append(socks, sockModel{slot: refCli, connected: true})
	nextSlot++
	ops = append(ops, udpClose(ephemSrv))
	socks[ephemSrv].closed = true
	ops = append(ops,
		udpConnWrite(refCli, pat(5, 0x22)),
		udpRead(refCli, 16, guardReady),
		udpRead(refCli, 16, 15*time.Millisecond),
	)

	for len(ops) < steps {
		switch rng.IntN(6) {
		case 0: // connected write -> srv0 readmsg
			sz := rng.IntN(256)
			capSz := max(1, sz)
			if rng.IntN(3) == 0 && sz > 4 {
				capSz = sz / 2
			}
			ops = append(ops,
				udpConnWrite(cli0, pat(sz, byte(len(ops)))),
				udpReadMsg(srv0, capSz, guardReady),
			)
		case 1: // unconnected sendto -> srv0 read
			sz := rng.IntN(128)
			ops = append(ops,
				udpSendToSlot(rpBase, srv0, pat(sz, byte(len(ops)))),
				udpRead(srv0, max(1, sz+4), guardReady),
			)
		case 2: // empty read timeout
			ops = append(ops, udpRead(srv0, 16, 10*time.Millisecond))
		case 3: // fresh listen + dial + exchange + close + post-close errors
			ops = append(ops, udpListen("udp4", "127.0.0.1:0", false, true))
			sSlot := nextSlot
			socks = append(socks, sockModel{slot: sSlot})
			nextSlot++
			ops = append(ops, udpDialSlot(sSlot, true))
			cSlot := nextSlot
			socks = append(socks, sockModel{slot: cSlot, connected: true})
			nextSlot++
			ops = append(ops,
				udpConnWrite(cSlot, pat(19, 0x77)),
				udpReadMsg(sSlot, 32, guardReady),
				udpClose(cSlot),
				udpClose(cSlot), // double close -> ErrClosed
				udpConnWrite(cSlot, []byte("after-close")),
				udpRead(cSlot, 8, 0),
				udpClose(sSlot),
			)
			socks[cSlot].closed = true
			socks[sSlot].closed = true
		case 4: // dual-stack wildcard listen + v4 sendto + readmsg
			ops = append(ops, udpListen("udp", ":0", false, true))
			dsSlot := nextSlot
			socks = append(socks, sockModel{slot: dsSlot})
			nextSlot++
			ops = append(ops,
				udpSendToSlot(srv0, dsSlot, pat(12, 0x44)),
				udpReadMsg(dsSlot, 32, guardReady),
				udpClose(dsSlot),
			)
			socks[dsSlot].closed = true
		case 5: // error probes
			kinds := []string{"unconnected-write", "sendto-port0", "set-buffers"}
			ops = append(ops, udpErrorProbe(kinds[rng.IntN(len(kinds))], srv0))
		}
	}
	return ops
}

func TestDSTConformanceUDP(t *testing.T) {
	allow := udpAllowlist()
	fired := make(map[string]int)
	for _, seed := range sweepSeeds(t) {
		ops := genUDPOps(seed, 90)
		host := runOpsHost(t, ops)
		sim := runOpsSim(t, seed, ops)
		if d := diffOutcomes(ops, host, sim, allow, fired); d != nil {
			reportDivergence(t, "udp", seed, ops, d)
			return
		}
	}
	checkAllowlistCoverage(t, allow, fired)
}

func TestDSTConformanceUDPCloseDuringRead(t *testing.T) {
	run := func() blockedResult {
		u, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
		if err != nil {
			return blockedResult{-1, "listen:" + errClass(err)}
		}
		go func() {
			time.Sleep(40 * time.Millisecond)
			_ = u.Close()
		}()
		u.SetReadDeadline(time.Now().Add(guardReady))
		n, _, rerr := u.ReadFromUDP(make([]byte, 8))
		return blockedResult{n, errClass(rerr)}
	}
	hostRes := run()
	if hostRes.n != 0 || hostRes.err != "OpError(read)/ErrClosed:net" {
		t.Fatalf("host outcome = %+v, want n=0 OpError(read)/ErrClosed:net", hostRes)
	}
	var simRes blockedResult
	simulation.Run(1, func() {
		simRes = run()
	})
	if simRes != hostRes {
		t.Fatalf("sim outcome = %+v, want %+v", simRes, hostRes)
	}
}
