// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package simulation

import (
	"context"
	crand "crypto/rand"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func parseCausalTimelineLines(t *testing.T, out string) []string {
	t.Helper()
	_, after, ok := strings.Cut(out, "causal event timeline:\n")
	if !ok {
		t.Fatalf("missing causal event timeline header:\n%s", out)
	}
	var lines []string
	for _, line := range strings.Split(after, "\n") {
		trimmed := strings.TrimSpace(line)
		// In child go test output, t.Logf prefixes continuation lines with whitespace.
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "... (") {
			lines = append(lines, trimmed)
			continue
		}
		break
	}
	if len(lines) == 0 {
		t.Fatalf("causal event timeline had no entries:\n%s", out)
	}
	return lines
}

var causalEntryRe = regexp.MustCompile(`^#(\d+) \+([^:]+): (.+)$`)

func verifyMonotonicTimeline(t *testing.T, lines []string) {
	t.Helper()
	var prevSeq uint64
	var prevAt time.Duration
	for i, line := range lines {
		if strings.HasPrefix(line, "... (") {
			if i != 0 {
				t.Fatalf("omitted-events notice not first line: %q", line)
			}
			continue
		}
		m := causalEntryRe.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("malformed causal timeline line %d: %q", i, line)
		}
		seq, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			t.Fatalf("bad seq in %q: %v", line, err)
		}
		at, err := time.ParseDuration(m[2])
		if err != nil {
			t.Fatalf("bad duration in %q: %v", line, err)
		}
		if prevSeq != 0 && seq != prevSeq+1 {
			t.Fatalf("non-consecutive sequence numbers %d -> %d in line %q", prevSeq, seq, line)
		}
		if prevSeq != 0 && at < prevAt {
			t.Fatalf("non-monotonic virtual timestamp +%s -> +%s in line %q", prevAt, at, line)
		}
		prevSeq = seq
		prevAt = at
	}
}

func TestDiagnosticsCausalTimelineTopologyFaultsAndExitShapes(t *testing.T) {
	if os.Getenv("GO_DST_DIAG_CAUSAL_TOPOLOGY_CHILD") == "1" {
		TestWith(t, 71, Options{CrashTear: true}, func(t *testing.T) {
			HostWith("n1", HostConfig{
				Hostname: "node-one.internal",
				Clock:    ClockConfig{offset: 25 * time.Millisecond, driftPPB: 500},
			}, func() {
				// 1. Normal ProcessWith in a named PID namespace.
				ProcessWith("init", ProcessConfig{PIDNamespace: "pod-alpha"}, func() {})

				// 2. Recovered panic inside Process still exits normally.
				Process("recovered-worker", func() {
					defer func() { _ = recover() }()
					panic("handled inside process")
				})

				// 3. Normal Process invoked from an outer panicking defer must still
				// record reason=normal (pinning dstCallerDeferPanicking frame depth).
				func() {
					defer func() {
						Process("defer-helper", func() {})
						_ = recover()
					}()
					panic("outer panic")
				}()

				// 4. Unrecovered panic escaping Process records reason=panic.
				func() {
					defer func() { _ = recover() }()
					Process("panicker", func() {
						panic("uncaught process panic")
					})
				}()

				// 5. Read-only mmap write fault records reason=mapping-fault.
				// A crashed process never runs defers, so synchronize via synctest.Wait().
				go func() {
					Process("mapper", func() {
						if err := os.WriteFile("/ro-map", make([]byte, 4096), 0o644); err != nil {
							panic(err)
						}
						f, err := os.Open("/ro-map")
						if err != nil {
							panic(err)
						}
						b, err := syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
						f.Close()
						if err != nil {
							panic(err)
						}
						b[0] = 0x7f // triggers simulated mapping fault teardown
					})
				}()
				synctest.Wait()

				// Prepare durable file for CorruptFile (and empty file for no-op check).
				f, err := os.OpenFile("/durable.bin", os.O_CREATE|os.O_RDWR, 0o644)
				if err != nil {
					t.Fatalf("open durable.bin: %v", err)
				}
				if _, err := f.Write([]byte("hello-platter")); err != nil {
					t.Fatalf("write durable.bin: %v", err)
				}
				if err := f.Sync(); err != nil {
					t.Fatalf("sync durable.bin: %v", err)
				}
				f.Close()
				ef, err := os.OpenFile("/empty.bin", os.O_CREATE|os.O_RDWR, 0o644)
				if err != nil {
					t.Fatalf("open empty.bin: %v", err)
				}
				if err := ef.Sync(); err != nil {
					t.Fatalf("sync empty.bin: %v", err)
				}
				ef.Close()
				rootDir, err := os.Open("/")
				if err != nil {
					t.Fatalf("open /: %v", err)
				}
				if err := rootDir.Sync(); err != nil {
					t.Fatalf("sync /: %v", err)
				}
				rootDir.Close()
			})

			// Live re-declaration of n1 updates clock without bumping boot count.
			time.Sleep(5 * time.Millisecond)
			HostWith("n1", HostConfig{Hostname: "node-one.internal"}, func() {})

			Host("n2", func() {})

			// Two concurrent sibling instances of "twin" on n2 -> CrashProcess(instances=2).
			twinReady := make(chan struct{}, 2)
			for range 2 {
				go Host("n2", func() {
					Process("twin", func() {
						twinReady <- struct{}{}
						select {}
					})
				})
			}
			<-twinReady
			<-twinReady
			time.Sleep(5 * time.Millisecond)
			ResetProcessConns("n2", "twin")
			CrashProcess("n2", "twin")

			// Exercise every network, link-policy, disk, and clock fault transition.
			time.Sleep(5 * time.Millisecond)
			Partition("n1", "n2")
			PartitionOneWay("n1", "n2")
			PartitionRefuse("n2", "n1")
			HealLink("n1", "n2")
			IsolateHost("n2")
			UnisolateHost("n2")
			ConfigureLink("n1", "n2", LinkConfig{Latency: 4 * time.Millisecond, Jitter: time.Millisecond, Bandwidth: 65536})
			ConfigureLinkOneWay("n2", "n1", LinkConfig{Latency: 8 * time.Millisecond})
			UnconfigureLink("n1", "n2")
			ResetLinkConns("n1", "n2")

			FailDisk("n1")
			HealDisk("n1")
			FailWriteback("n1")
			HealWriteback("n1")
			FailFile("n1", "/durable.bin")
			HealFile("n1", "/durable.bin")
			LimitDisk("n1", 8192)
			UnlimitDisk("n1")
			SlowDisk("n1", 2*time.Millisecond)
			UnslowDisk("n1")
			StepClock("n1", 50*time.Millisecond)
			DriftClock("n1", -250)

			// CorruptFile on empty/missing targets is a no-op (no causal event);
			// CorruptFile on durable.bin flips a bit on live then offline platter.
			CorruptFile("n1", "/empty.bin")
			CorruptFile("n1", "/no-such-file")
			CorruptFile("n1", "/durable.bin")

			CrashHost("n1")
			CorruptFile("n1", "/durable.bin")
			Host("n1", func() {}) // reboot -> boot=2

			t.Fatal("inspect topology and fault causal timeline")
		})
		return
	}

	if os.Getenv("GO_DST_DIAG_CAUSAL_RESTART_CHILD") == "1" {
		WorldTest(t, 72, Options{}, []HostDecl{
			{Name: "w1", Boot: func() {}},
			{Name: "w2", Boot: func() {}},
		}, func(t *testing.T, ctl *Ctl) {
			CrashHost("w1")
			// RestartHost on an already-down host must NOT emit a duplicate CrashHost.
			ctl.RestartHost("w1")
			// RestartHost on a live host emits CrashHost + Host boot (boot=2).
			ctl.RestartHost("w2")
			t.Fatal("inspect WorldTest RestartHost and Ctl.End suppression")
		})
		return
	}

	out := runDiagnosticsChild(t, "TestDiagnosticsCausalTimelineTopologyFaultsAndExitShapes", "GO_DST_DIAG_CAUSAL_TOPOLOGY_CHILD", true)
	lines := parseCausalTimelineLines(t, out)
	verifyMonotonicTimeline(t, lines)
	joined := strings.Join(lines, "\n")

	wantSubstrings := []string{
		`Host boot "n1" (boot=1 hostname="node-one.internal"`,
		`Process start [host="n1" proc="init"`,
		`pidns="pod-alpha"]`,
		`Process exit [host="n1" proc="init"`,
		`(reason=normal)`,
		`Process exit [host="n1" proc="recovered-worker"`,
		`Process exit [host="n1" proc="defer-helper"`,
		`Process exit [host="n1" proc="panicker"`,
		`Process exit [host="n1" proc="mapper"`,
		`(reason=mapping-fault)`,
		`Host clock re-declared "n1"`,
		`ResetProcessConns [host="n2" proc="twin"] (instances=2)`,
		`CrashProcess [host="n2" proc="twin"] (instances=2)`,
		`Partition "n1" <-> "n2"`,
		`PartitionOneWay "n1" -> "n2"`,
		`PartitionRefuse "n2" <-> "n1"`,
		`HealLink "n1" <-> "n2"`,
		`IsolateHost "n2"`,
		`UnisolateHost "n2"`,
		`ConfigureLink "n1" <-> "n2" (latency=4ms jitter=1ms bandwidth=65536B/s)`,
		`ConfigureLinkOneWay "n2" -> "n1" (latency=8ms jitter=0s bandwidth=0B/s)`,
		`UnconfigureLink "n1" <-> "n2"`,
		`ResetLinkConns "n1" <-> "n2"`,
		`FailDisk "n1"`,
		`HealDisk "n1"`,
		`FailWriteback "n1"`,
		`HealWriteback "n1"`,
		`FailFile "n1" path="/durable.bin"`,
		`HealFile "n1" path="/durable.bin"`,
		`LimitDisk "n1" (bytes=8192)`,
		`UnlimitDisk "n1"`,
		`SlowDisk "n1" (perOp=2ms)`,
		`UnslowDisk "n1"`,
		`StepClock "n1" (delta=50ms)`,
		`DriftClock "n1" (ppb=-250)`,
		`CorruptFile "n1" path="/durable.bin"`,
		`image=durable)`,
		`CrashHost "n1" (procs=0 crashTear=true)`,
		`image=offline-platter)`,
		`Host boot "n1" (boot=2)`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in timeline:\n%s", want, joined)
		}
	}
	for _, normalProc := range []string{"recovered-worker", "defer-helper"} {
		for _, line := range lines {
			if strings.Contains(line, "Process exit") && strings.Contains(line, `proc="`+normalProc+`"`) && !strings.Contains(line, "reason=normal") {
				t.Errorf("%s exit classified wrongly: %s", normalProc, line)
			}
		}
	}
	for _, line := range lines {
		if strings.Contains(line, "Process exit") && strings.Contains(line, `proc="panicker"`) && !strings.Contains(line, "reason=panic") {
			t.Errorf("panicker exit classified wrongly: %s", line)
		}
		if strings.Contains(line, "CorruptFile") && (strings.Contains(line, "empty.bin") || strings.Contains(line, "no-such-file")) {
			t.Errorf("no-op CorruptFile unexpectedly recorded: %s", line)
		}
	}

	restartOut := runDiagnosticsChild(t, "TestDiagnosticsCausalTimelineTopologyFaultsAndExitShapes", "GO_DST_DIAG_CAUSAL_RESTART_CHILD", true)
	restartLines := parseCausalTimelineLines(t, restartOut)
	verifyMonotonicTimeline(t, restartLines)
	w1Crashes, w2Crashes := 0, 0
	for _, line := range restartLines {
		if strings.Contains(line, `CrashHost "w1"`) {
			w1Crashes++
		}
		if strings.Contains(line, `CrashHost "w2"`) {
			w2Crashes++
		}
	}
	if w1Crashes != 1 {
		t.Errorf("CrashHost \"w1\" count = %d, want 1 (RestartHost on down host + Ctl.End must not duplicate):\n%s", w1Crashes, strings.Join(restartLines, "\n"))
	}
	if w2Crashes != 1 {
		t.Errorf("CrashHost \"w2\" count = %d, want 1 (Ctl.End must be suppressed):\n%s", w2Crashes, strings.Join(restartLines, "\n"))
	}
}

func TestDiagnosticsCausalTimelineTCPAndFilesystemTransitions(t *testing.T) {
	if os.Getenv("GO_DST_DIAG_CAUSAL_TCP_FS_CHILD") == "1" {
		TestWith(t, 73, Options{
			Network: NetworkConfig{
				RetransmitTimeout: 40 * time.Millisecond,
			},
		}, func(t *testing.T) {
			srvReady := make(chan struct{})
			retransDone := make(chan struct{})
			kaDone := make(chan struct{})
			rstDone := make(chan struct{})

			Host("srv", func() {
				go Process("api-srv", func() {
					ln, err := net.Listen("tcp", "0.0.0.0:9100")
					if err != nil {
						panic(err)
					}
					lnKA, err := (&net.ListenConfig{
						KeepAliveConfig: net.KeepAliveConfig{
							Enable:   true,
							Idle:     20 * time.Millisecond,
							Interval: 10 * time.Millisecond,
							Count:    2,
						},
					}).Listen(context.Background(), "tcp", "0.0.0.0:9101")
					if err != nil {
						panic(err)
					}
					lnBacklog, err := net.Listen("tcp", "0.0.0.0:9102")
					if err != nil {
						panic(err)
					}
					close(srvReady)

					// Conn 1: steady-state traffic + ResetLinkConns RST.
					c1, err := ln.Accept()
					if err != nil {
						panic(err)
					}
					var buf [16]byte
					if _, err := c1.Read(buf[:4]); err != nil {
						panic(err)
					}
					if _, err := c1.Write([]byte("pong")); err != nil {
						panic(err)
					}

					// Conn 2: retransmit horizon ETIMEDOUT.
					c2, err := ln.Accept()
					if err != nil {
						panic(err)
					}
					_ = c2

					// Conn 3: keepalive ETIMEDOUT.
					c3, err := lnKA.Accept()
					if err != nil {
						panic(err)
					}
					go func() {
						defer close(kaDone)
						var kb [4]byte
						_, _ = c3.Read(kb[:])
					}()

					// Conn 4: close-with-unread-data emits RST to client.
					c4, err := ln.Accept()
					if err != nil {
						panic(err)
					}
					// Wait until client's unread bytes are queued, then close without draining.
					<-rstDone
					_ = c4.Close()

					<-retransDone
					<-kaDone
					// Close lnBacklog while one unaccepted established dial sits in its backlog!
					_ = lnBacklog.Close()
					_ = lnKA.Close()
					_ = ln.Close()
				})
			})

			<-srvReady
			srvAddr := HostIP("srv") + ":9100"
			kaAddr := HostIP("srv") + ":9101"
			backlogAddr := HostIP("srv") + ":9102"
			Host("cli", func() {
				Process("api-cli", func() {
					// Refused dial to closed port (must NOT emit TCP RST delivered).
					_, _ = net.Dial("tcp", HostIP("srv")+":9199")

					// Conn 1: steady-state Ping/Pong + ResetLinkConns.
					c1, err := net.Dial("tcp", srvAddr)
					if err != nil {
						t.Fatalf("dial c1: %v", err)
					}
					if _, err := c1.Write([]byte("ping")); err != nil {
						t.Fatalf("write c1: %v", err)
					}
					var pong [4]byte
					if _, err := c1.Read(pong[:]); err != nil {
						t.Fatalf("read c1: %v", err)
					}
					ResetLinkConns("cli", "srv")
					// Consuming ECONNRESET after ResetLinkConns must NOT double-log RST!
					_, _ = c1.Read(pong[:])
					_ = c1.Close()

					// Conn 2: retransmit ETIMEDOUT across Partition.
					c2, err := net.Dial("tcp", srvAddr)
					if err != nil {
						t.Fatalf("dial c2: %v", err)
					}
					// Conn 3: keepalive ETIMEDOUT across Partition.
					c3, err := net.Dial("tcp", kaAddr)
					if err != nil {
						t.Fatalf("dial c3: %v", err)
					}
					// Conn 4: server closes with unread bytes -> RST delivered to c4.
					c4, err := net.Dial("tcp", srvAddr)
					if err != nil {
						t.Fatalf("dial c4: %v", err)
					}
					_, _ = c4.Write([]byte("unread"))
					close(rstDone)
					time.Sleep(time.Millisecond)
					_, _ = c4.Read(pong[:])
					_ = c4.Close()

					// Backlog conn: Dial succeeds into lnBacklog without Accept, then server closes lnBacklog -> backlog RST!
					cBacklog, err := net.Dial("tcp", backlogAddr)
					if err != nil {
						t.Fatalf("dial backlog: %v", err)
					}

					Partition("cli", "srv")
					// Deadline dial timeout + retransmit dial timeout across Partition.
					_, _ = net.DialTimeout("tcp", srvAddr, 5*time.Millisecond)
					_, _ = net.Dial("tcp", srvAddr)

					// Trigger c2 retransmit timeout and c3 keepalive timeout (Linux TCP_KEEPIDLE/INTVL
					// are whole-second socket options: 1s idle + 2*1s interval = 3s virtual time).
					_, _ = c2.Write([]byte("stalled"))
					time.Sleep(4 * time.Second)
					_, _ = c2.Read(pong[:])
					<-kaDone
					HealLink("cli", "srv")
					close(retransDone)
					time.Sleep(5 * time.Millisecond)
					_, _ = cBacklog.Read(pong[:])
					_ = cBacklog.Close()
					_ = c2.Close()
					_ = c3.Close()

					// Filesystem fault transitions on cli: EIO, fsyncgate, ENOSPC.
					f, err := os.OpenFile("/wal.log", os.O_CREATE|os.O_RDWR, 0o644)
					if err != nil {
						t.Fatalf("create wal.log: %v", err)
					}
					_, _ = f.Write([]byte("clean-prefix"))
					_ = f.Sync()

					// FailWriteback -> Sync fails with EIO + fsyncgate drop; O_SYNC write fails with EIO + fsyncgate drop.
					FailWriteback("cli")
					_, _ = f.Write([]byte("dirty"))
					_ = f.Sync()
					_ = syscall.Fdatasync(int(f.Fd()))
					osyncF, err := os.OpenFile("/osync.log", os.O_CREATE|os.O_RDWR|os.O_SYNC, 0o644)
					if err != nil {
						t.Fatalf("open osync.log: %v", err)
					}
					_, _ = osyncF.Write([]byte("sync-write"))
					_, _ = osyncF.WriteAt([]byte("sync-pwrite"), 0)
					osyncF.Close()
					HealWriteback("cli")

					// FailFile -> read, pread, write, pwrite, fallocate, mmap EIO, plus 0-byte raw Pread/Pwrite op attribution.
					FailFile("cli", "/wal.log")
					var rb [4]byte
					_, _ = f.Read(rb[:])
					_, _ = f.ReadAt(rb[:], 0)
					_, _ = f.Write([]byte("x"))
					_, _ = f.WriteAt([]byte("x"), 0)
					_ = syscall.Fallocate(int(f.Fd()), 0, 0, 4096)
					_, _ = syscall.Mmap(int(f.Fd()), 0, 4096, syscall.PROT_READ, syscall.MAP_SHARED)
					HealFile("cli", "/wal.log")
					f.Close()

					zeroF, err := os.OpenFile("/zero-io.log", os.O_CREATE|os.O_RDWR, 0o644)
					if err != nil {
						t.Fatalf("create zero-io.log: %v", err)
					}
					FailFile("cli", "/zero-io.log")
					_, _ = syscall.Pread(int(zeroF.Fd()), nil, 0)
					_, _ = syscall.Pwrite(int(zeroF.Fd()), nil, 0)
					HealFile("cli", "/zero-io.log")
					zeroF.Close()

					if err := os.Mkdir("/root-existing", 0o755); err != nil {
						t.Fatalf("mkdir /root-existing: %v", err)
					}

					// LimitDisk -> partial write ENOSPC, outright write ENOSPC, fallocate ENOSPC, create/mkdir/os.Root ENOSPC.
					// Currently /wal.log (1 page) + /osync.log (1 page) = 2 pages (8192B); allow 3 pages (12288B) -> 1 free page (4096B).
					LimitDisk("cli", 3*4096)
					fill, err := os.OpenFile("/fill.bin", os.O_CREATE|os.O_RDWR, 0o644)
					if err != nil {
						t.Fatalf("create fill.bin: %v", err)
					}
					// Request 8192 bytes with 4096 free -> short write (4096, ENOSPC)!
					_, _ = fill.Write(make([]byte, 8192))
					// Now 0 free blocks -> outright write, fallocate, create, mkdir, and os.Root fail ENOSPC.
					_, _ = fill.Write([]byte("full"))
					_ = syscall.Fallocate(int(fill.Fd()), 0, 4096, 8192)
					fill.Close()
					_, _ = os.OpenFile("/denied.bin", os.O_CREATE|os.O_RDWR, 0o644)
					_ = os.Mkdir("/denied-dir", 0o755)
					rootH, err := os.OpenRoot("/")
					if err != nil {
						t.Fatalf("OpenRoot: %v", err)
					}
					_, _ = rootH.OpenFile("root-denied.bin", os.O_CREATE|os.O_RDWR, 0o644)
					_ = rootH.Mkdir("root-denied-dir", 0o755)
					_ = rootH.MkdirAll("root-existing/mid/leaf", 0o755)
					rootH.Close()

					// File.WriteAt partial fill (1 free page, 2-page WriteAt) must emit exactly ONE FS ENOSPC event.
					LimitDisk("cli", 4*4096)
					fillAt, err := os.OpenFile("/fill-at.bin", os.O_CREATE|os.O_RDWR, 0o644)
					if err != nil {
						t.Fatalf("create fill-at.bin: %v", err)
					}
					_, _ = fillAt.WriteAt(make([]byte, 8192), 0)
					fillAt.Close()

					// Unretried raw syscall.Write and syscall.Pwrite short fills return (4096, nil) without ENOSPC
					// and must NOT prematurely emit FS ENOSPC causal events.
					LimitDisk("cli", 5*4096)
					rawW, err := os.OpenFile("/raw-write.bin", os.O_CREATE|os.O_RDWR, 0o644)
					if err != nil {
						t.Fatalf("create raw-write.bin: %v", err)
					}
					if nw, werr := syscall.Write(int(rawW.Fd()), make([]byte, 8192)); nw != 4096 || werr != nil {
						t.Fatalf("raw syscall.Write short fill = (%d, %v), want (4096, nil)", nw, werr)
					}
					rawW.Close()

					LimitDisk("cli", 6*4096)
					rawP, err := os.OpenFile("/raw-pwrite.bin", os.O_CREATE|os.O_RDWR, 0o644)
					if err != nil {
						t.Fatalf("create raw-pwrite.bin: %v", err)
					}
					if np, perr := syscall.Pwrite(int(rawP.Fd()), make([]byte, 8192), 0); np != 4096 || perr != nil {
						t.Fatalf("raw syscall.Pwrite short fill = (%d, %v), want (4096, nil)", np, perr)
					}
					rawP.Close()
					UnlimitDisk("cli")
				})
			})

			t.Fatal("inspect TCP and FS causal timeline")
		})
		return
	}

	out := runDiagnosticsChild(t, "TestDiagnosticsCausalTimelineTCPAndFilesystemTransitions", "GO_DST_DIAG_CAUSAL_TCP_FS_CHILD", true)
	lines := parseCausalTimelineLines(t, out)
	verifyMonotonicTimeline(t, lines)
	joined := strings.Join(lines, "\n")

	wantSubstrings := []string{
		`TCP Listen [host="srv" proc="api-srv"`,
		`addr="0.0.0.0:9100" (port=9100)`,
		`TCP Dial refused [host="cli" proc="api-cli"`,
		`:9199"`,
		`TCP Dial established [host="cli" proc="api-cli"`,
		`-> "srv":9100`,
		`TCP RST delivered "cli":`,
		`TCP RST delivered "srv":9100 <- "cli":`,
		`TCP Dial timeout [host="cli" proc="api-cli"`,
		`reason=deadline)`,
		`reason=retransmit)`,
		`TCP retransmit ETIMEDOUT "cli":`,
		`TCP keepalive ETIMEDOUT "srv":9101 -> "cli":`,
		`TCP Listener.Close [host="srv" proc="api-srv"`,
		`FS EIO [host="cli" proc="api-cli"`,
		`op=fsync path="/wal.log"`,
		`op=fdatasync path="/wal.log"`,
		`op=read path="/wal.log"`,
		`op=pread path="/wal.log"`,
		`op=write path="/wal.log"`,
		`op=pwrite path="/wal.log"`,
		`op=fallocate path="/wal.log"`,
		`op=mmap path="/wal.log"`,
		`op=pread path="/zero-io.log"`,
		`op=pwrite path="/zero-io.log"`,
		`FS fsyncgate writeback drop [host="cli" proc="api-cli"`,
		`op=write path="/osync.log"`,
		`op=pwrite path="/osync.log"`,
		`FS ENOSPC [host="cli" proc="api-cli"`,
		`op=write path="/fill.bin" (written=4096 requested=8192)`,
		`op=write path="/fill.bin" (written=0 requested=4)`,
		`op=fallocate path="/fill.bin" (written=0 requested=8192)`,
		`op=create path="/denied.bin"`,
		`op=mkdir path="/denied-dir"`,
		`op=create path="root-denied.bin"`,
		`op=mkdir path="root-denied-dir"`,
		`op=mkdir path="root-existing/mid"`,
		`op=pwrite path="/fill-at.bin" (written=0 requested=4096)`,
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in timeline:\n%s", want, joined)
		}
	}

	// Verify Listener.Close resolved proc="api-srv" (never proc#<slot>),
	// c1's RST on "cli" was logged once despite post-reset Read consuming ECONNRESET,
	// File.WriteAt partial fill logged ENOSPC once, and raw short syscalls / Root.MkdirAll leaf did not false-log.
	c1PortRe := regexp.MustCompile(`TCP Dial established \[[^\]]+\] :(\d+) -> "srv":9100`)
	m := c1PortRe.FindStringSubmatch(joined)
	if m == nil {
		t.Fatalf("could not extract c1 local port:\n%s", joined)
	}
	c1Port := m[1]
	c1RSTCount := 0
	fillAtENOSPC := 0
	for _, line := range lines {
		if strings.Contains(line, "proc#") {
			t.Errorf("unresolved numeric proc# leaked into causal timeline (slot vs logical proc bug): %s", line)
		}
		if strings.Contains(line, fmt.Sprintf(`TCP RST delivered "cli":%s <- "srv":9100`, c1Port)) {
			c1RSTCount++
		}
		if strings.Contains(line, "9199") && strings.Contains(line, "TCP RST delivered") {
			t.Errorf("refused dial on 9199 falsely emitted TCP RST delivered: %s", line)
		}
		if strings.Contains(line, `FS ENOSPC`) && strings.Contains(line, `"/fill-at.bin"`) {
			fillAtENOSPC++
		}
		if strings.Contains(line, `"/raw-write.bin"`) || strings.Contains(line, `"/raw-pwrite.bin"`) {
			t.Errorf("unretried raw short syscall falsely emitted FS ENOSPC: %s", line)
		}
		if strings.Contains(line, `"root-existing/mid/leaf"`) {
			t.Errorf("Root.MkdirAll logged full leaf instead of failing intermediate dir: %s", line)
		}
		if strings.Contains(line, `op=read path="/zero-io.log"`) || strings.Contains(line, `op=write path="/zero-io.log"`) {
			t.Errorf("0-byte Pread/Pwrite misattributed as read/write: %s", line)
		}
	}
	if c1RSTCount != 1 {
		t.Errorf("c1 RST count = %d, want 1:\n%s", c1RSTCount, joined)
	}
	if fillAtENOSPC != 1 {
		t.Errorf("fill-at.bin ENOSPC count = %d, want 1:\n%s", fillAtENOSPC, joined)
	}
}

func TestDiagnosticsCausalTimelineWrapTruncationResetAndExplore(t *testing.T) {
	if os.Getenv("GO_DST_DIAG_CAUSAL_WRAP_CHILD") == "1" {
		// Passing run beforehand with a distinct sentinel host must NOT leak into the failing Test below.
		Run(74, func() {
			Host("leaked-prev-run-host", func() {
				StepClock("leaked-prev-run-host", time.Second)
			})
		})
		longNS := strings.Repeat("n", 190)
		longPath := "/" + strings.Repeat("p", 199)
		Test(t, 75, func(t *testing.T) {
			Host("wrap-host", func() {
				ProcessWith("trunc-proc", ProcessConfig{PIDNamespace: longNS}, func() {})
			})
			FailFile("wrap-host", longPath)
			for range 300 {
				time.Sleep(time.Millisecond)
				StepClock("wrap-host", time.Millisecond)
			}
			t.Fatal("trigger wrap check")
		})
		return
	}

	if os.Getenv("GO_DST_DIAG_CAUSAL_TRUNC_ONLY_CHILD") == "1" {
		longNS := strings.Repeat("n", 190)
		longPath := "/" + strings.Repeat("p", 199)
		Test(t, 76, func(t *testing.T) {
			Host("trunc-host", func() {
				ProcessWith("trunc-proc", ProcessConfig{PIDNamespace: longNS}, func() {})
			})
			FailFile("trunc-host", longPath)
			t.Fatal("trigger truncation check")
		})
		return
	}

	if os.Getenv("GO_DST_DIAG_CAUSAL_EXPLORE_CHILD") == "1" {
		ExploreTest(t, 77, ExploreOptions{MaxSchedules: 4}, func() error {
			Host("exp-node", func() {
				Process("exp-worker", func() {})
			})
			StepClock("exp-node", 3*time.Millisecond)
			return errors.New("explore invariant broken")
		})
		return
	}

	wrapOut := runDiagnosticsChild(t, "TestDiagnosticsCausalTimelineWrapTruncationResetAndExplore", "GO_DST_DIAG_CAUSAL_WRAP_CHILD", true)
	if strings.Contains(wrapOut, "leaked-prev-run-host") {
		t.Fatalf("earlier passing Run leaked causal events into later failing Test:\n%s", wrapOut)
	}
	wrapLines := parseCausalTimelineLines(t, wrapOut)
	verifyMonotonicTimeline(t, wrapLines)
	// Total events = 1 (Host boot) + 1 (Process start) + 1 (Process exit) + 1 (FailFile) + 300 (StepClock) = 304 -> 48 omitted, 256 shown (#49..#304).
	if wrapLines[0] != "... (48 earlier causal event(s) omitted; showing last 256)" {
		t.Fatalf("unexpected wrap header: %q", wrapLines[0])
	}
	if len(wrapLines) != 1+256 {
		t.Fatalf("wrapped line count = %d, want 257", len(wrapLines))
	}
	if !strings.HasPrefix(wrapLines[1], "#49 ") || !strings.HasPrefix(wrapLines[len(wrapLines)-1], "#304 ") {
		t.Fatalf("unexpected wrapped sequence bounds: first=%q last=%q", wrapLines[1], wrapLines[len(wrapLines)-1])
	}

	truncOut := runDiagnosticsChild(t, "TestDiagnosticsCausalTimelineWrapTruncationResetAndExplore", "GO_DST_DIAG_CAUSAL_TRUNC_ONLY_CHILD", true)
	if !strings.Contains(truncOut, `(truncated from 190 bytes)`) || !strings.Contains(truncOut, `(truncated from 200 bytes)`) {
		t.Fatalf("missing >128B truncation markers:\n%s", truncOut)
	}

	exploreOut := runDiagnosticsChild(t, "TestDiagnosticsCausalTimelineWrapTruncationResetAndExplore", "GO_DST_DIAG_CAUSAL_EXPLORE_CHILD", true)
	if !strings.Contains(exploreOut, "causal event timeline:\n") ||
		!strings.Contains(exploreOut, `Host boot "exp-node"`) ||
		!strings.Contains(exploreOut, `StepClock "exp-node" (delta=3ms)`) {
		t.Fatalf("ExploreTest failure missing causal event timeline:\n%s", exploreOut)
	}
}

func TestDiagnosticsCausalTimelineObservationNeutrality(t *testing.T) {
	type runObs struct {
		mathDraw   uint64
		cryptoHex  [16]byte
		rttNs      int64
		numGC      uint32
		procAlloc  int64
		traceLen   int
		traceHead  [16]uint64
		causalSeen int
	}

	execute := func(suppress bool) runObs {
		var obs runObs
		RunWith(88, Options{
			MemoryLimit: 1 << 20,
			Network: NetworkConfig{
				CrossHost: LinkConfig{
					Latency: 2 * time.Millisecond,
					Jitter:  3 * time.Millisecond,
				},
			},
		}, func() {
			if suppress {
				dstCausalSuppressEnter()
				defer dstCausalSuppressExit()
			}
			var gcBefore, gcAfter runtime.MemStats
			runtime.ReadMemStats(&gcBefore)

			ready := make(chan string, 1)
			done := make(chan struct{})
			Host("srv", func() {
				go Process("echo", func() {
					defer close(done)
					ln, err := net.Listen("tcp", "0.0.0.0:9300")
					if err != nil {
						panic(err)
					}
					ready <- HostIP("srv") + ":9300"
					c, err := ln.Accept()
					if err != nil {
						panic(err)
					}
					var b [4]byte
					_, _ = c.Read(b[:])
					_, _ = c.Write(b[:])
					_ = c.Close()
					_ = ln.Close()
				})
			})

			addr := <-ready
			Host("cli", func() {
				Process("driver", func() {
					// Overflow the 256-slot ring buffer mid-run while also exercising FS, TCP, GC, and RNGs.
					for i := range 280 {
						StepClock("cli", time.Microsecond)
						sink := make([]byte, 4096)
						sink[0] = byte(i)
					}
					FailDisk("cli")
					f, _ := os.OpenFile("/probe", os.O_CREATE|os.O_RDWR, 0o644)
					if f != nil {
						_, _ = f.Write([]byte("x"))
						f.Close()
					}
					HealDisk("cli")

					t0 := time.Now()
					c, err := net.Dial("tcp", addr)
					if err != nil {
						panic(err)
					}
					_, _ = c.Write([]byte("ping"))
					var pong [4]byte
					_, _ = c.Read(pong[:])
					obs.rttNs = time.Since(t0).Nanoseconds()
					_ = c.Close()

					obs.mathDraw = rand.Uint64()
					_, _ = crand.Read(obs.cryptoHex[:])
				})
				obs.procAlloc = dstProcAllocBytes(lookupProc(lookupHost("cli"), "driver"))
			})
			<-done
			runtime.ReadMemStats(&gcAfter)
			obs.numGC = gcAfter.NumGC - gcBefore.NumGC
		})
		obs.traceLen = dstTraceLenFP()
		for i := 0; i < len(obs.traceHead) && i < obs.traceLen; i++ {
			obs.traceHead[i] = dstTraceChosenFP(i)
		}
		_, _, _, _, _, _, _, _, _, _, _, _, causalMeta, _, _ := dstSnapshotRunDiagnostics()
		obs.causalSeen = len(causalMeta)
		return obs
	}

	// Warm process-once stdlib sync.Once tables (e.g., net resolver config) before
	// comparing exact per-process byte allocation counters across runs.
	_ = execute(true)
	recorded := execute(false)
	suppressed := execute(true)
	if recorded.causalSeen != 256 {
		t.Fatalf("recorded causalSeen = %d, want 256 (wrapped ring)", recorded.causalSeen)
	}
	if suppressed.causalSeen != 0 {
		t.Fatalf("suppressed causalSeen = %d, want 0", suppressed.causalSeen)
	}
	recorded.causalSeen = 0
	if recorded != suppressed {
		t.Fatalf("causal event recording perturbed execution:\nrecorded:   %+v\nsuppressed: %+v", recorded, suppressed)
	}

	// ExploreWith(DPOR) schedule count, footprint, and Level-2 access yield count
	// must also be strictly identical with causal recording active vs suppressed.
	exploreWorkload := func(suppress bool) (ExploreResult, uint64) {
		beforeYields := dstAccessYieldFP()
		r := ExploreWith(89, ExploreOptions{Mode: DPOR, MaxSchedules: 16}, func() error {
			if suppress {
				dstCausalSuppressEnter()
				defer dstCausalSuppressExit()
			}
			var mu sync.Mutex
			var shared int
			start := make(chan struct{})
			ch := make(chan struct{}, 2)
			Host("h1", func() {
				go Process("p1", func() {
					<-start
					StepClock("h1", time.Millisecond)
					_ = os.WriteFile("/f", []byte("A"), 0o600)
					mu.Lock()
					shared = 1
					mu.Unlock()
					ch <- struct{}{}
				})
				go Process("p2", func() {
					<-start
					FailDisk("h1")
					HealDisk("h1")
					_ = os.WriteFile("/f", []byte("B"), 0o600)
					mu.Lock()
					shared = 2
					mu.Unlock()
					ch <- struct{}{}
				})
				close(start)
			})
			<-ch
			<-ch
			mu.Lock()
			_ = shared
			mu.Unlock()
			return nil
		})
		return r, dstAccessYieldFP() - beforeYields
	}

	repOn, yieldsOn := exploreWorkload(false)
	repOff, yieldsOff := exploreWorkload(true)
	if repOn.Schedules <= 1 {
		t.Fatalf("expected DPOR to explore >1 schedule on conflicting FS/mutex accesses, got %d", repOn.Schedules)
	}
	if repOn.Schedules != repOff.Schedules ||
		repOn.Footprint != repOff.Footprint ||
		repOn.UnattributedRaces != repOff.UnattributedRaces ||
		repOn.ForeignSched != repOff.ForeignSched ||
		yieldsOn != yieldsOff {
		t.Fatalf("DPOR exploration perturbed by causal recording:\n on:  schedules=%d footprint=%+v races=%d foreign=%v accessYields=%d\n off: schedules=%d footprint=%+v races=%d foreign=%v accessYields=%d",
			repOn.Schedules, repOn.Footprint, repOn.UnattributedRaces, repOn.ForeignSched, yieldsOn,
			repOff.Schedules, repOff.Footprint, repOff.UnattributedRaces, repOff.ForeignSched, yieldsOff)
	}
}
