// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst && linux

package os_test

import (
	"bytes"
	"errors"
	"math/bits"
	"os"
	"syscall"
	"testing"
	"testing/simulation"
)

func fstatBlocks(t *testing.T, f *os.File) (size, blocks int64) {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Fstat(int(f.Fd()), &st); err != nil {
		t.Fatalf("fstat: %v", err)
	}
	return st.Size, st.Blocks
}

// TestDSTDiskENOSPCSparseTruncateAndHoleWrite pins 4 KB allocated-block
// accounting under LimitDisk: sparse Truncate-grow charges 0 blocks, hole-filling
// writes charge 4 KB per newly touched page (failing mid-file with ENOSPC when
// the disk fills), in-place overwrites of allocated blocks charge 0 new blocks,
// multi-page writes clip at exact 4 KB boundaries across mixed allocated/hole
// spans, and Truncate-down frees allocated blocks immediately.
func TestDSTDiskENOSPCSparseTruncateAndHoleWrite(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			simulation.LimitDisk("h", 2*4096) // 2 x 4 KB blocks total

			f, err := os.Create("/sparse")
			mustOK(t, "create", err)
			defer f.Close()

			// 1 MiB sparse preallocation allocates 0 blocks.
			mustOK(t, "sparse truncate", f.Truncate(1<<20))
			sz, blks := fstatBlocks(t, f)
			if sz != 1<<20 || blks != 0 {
				t.Fatalf("after sparse truncate: size=%d blocks=%d, want %d, 0", sz, blks, 1<<20)
			}

			// Writing into hole page 128 (512 KiB) charges 1 block (8 x 512B sectors).
			_, err = f.WriteAt([]byte("page-128"), 512<<10)
			mustOK(t, "write hole page 128", err)
			if _, blks = fstatBlocks(t, f); blks != 8 {
				t.Fatalf("after first hole write: blocks=%d, want 8", blks)
			}

			// Overwriting an already-allocated page charges 0 additional blocks.
			_, err = f.WriteAt([]byte("overwrite"), (512<<10)+64)
			mustOK(t, "overwrite page 128", err)
			if _, blks = fstatBlocks(t, f); blks != 8 {
				t.Fatalf("after in-place overwrite: blocks=%d, want 8", blks)
			}

			// Writing into hole page 0 charges the 2nd (and final) block.
			_, err = f.WriteAt([]byte("page-0"), 0)
			mustOK(t, "write hole page 0", err)
			if _, blks = fstatBlocks(t, f); blks != 16 {
				t.Fatalf("after second hole write: blocks=%d, want 16", blks)
			}

			// Mid-file pwrite into hole page 1 (offset 4096 < Size!) fails ENOSPC.
			n, err := f.WriteAt([]byte("blocked-hole"), 4096)
			if n != 0 || !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("hole write on full disk: got (%d, %v), want (0, ENOSPC)", n, err)
			}
			holeBuf := make([]byte, 4096)
			_, err = f.ReadAt(holeBuf, 4096)
			mustOK(t, "read refused hole", err)
			if !bytes.Equal(holeBuf, make([]byte, 4096)) {
				t.Fatalf("refused hole write dirtied page 1")
			}

			// Raise cap to 3 blocks (1 free block) and issue a 9000-byte write
			// across [allocated page 0 | hole page 1 | hole page 2]: page 0 needs
			// 0 blocks, page 1 consumes the 1 free block, and page 2 clips at 8192
			// with ENOSPC.
			simulation.LimitDisk("h", 3*4096)
			payload := bytes.Repeat([]byte{0xab}, 9000)
			n, err = f.WriteAt(payload, 0)
			if n != 8192 || !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("spanning partial hole fill: got (%d, %v), want (8192, ENOSPC)", n, err)
			}
			if _, blks = fstatBlocks(t, f); blks != 24 {
				t.Fatalf("after partial fill: blocks=%d, want 24", blks)
			}
			got := make([]byte, 12288)
			_, err = f.ReadAt(got, 0)
			mustOK(t, "read back partial fill", err)
			if !bytes.Equal(got[:8192], payload[:8192]) {
				t.Fatalf("pages 0..1 content mismatch after partial fill")
			}
			if !bytes.Equal(got[8192:], make([]byte, 4096)) {
				t.Fatalf("unallocated page 2 dirtied past clip boundary")
			}

			// Truncating down to 4096 drops blocks on pages 1 and 128, freeing 2 blocks.
			mustOK(t, "truncate down", f.Truncate(4096))
			if _, blks = fstatBlocks(t, f); blks != 8 {
				t.Fatalf("after truncate down: blocks=%d, want 8", blks)
			}
			mustOK(t, "write 8 KiB sibling into freed blocks", os.WriteFile("/sib", make([]byte, 8192), 0o644))
		})
	})
}

// TestDSTDiskENOSPCMmapDirtyHoleAndReadImmunity verifies that reading sparse
// holes through PROT_READ or PROT_WRITE mappings never charges disk blocks, while
// dirtying a hole page via PROT_WRITE mmap (including after mprotect upgrade)
// folds into allocated block accounting.
func TestDSTDiskENOSPCMmapDirtyHoleAndReadImmunity(t *testing.T) {
	if ^uint(0)>>63 == 0 {
		t.Skip("simulated mmap requires 64-bit page-cache region")
	}
	simulation.Run(1, func() {
		onHost("h", func() {
			simulation.LimitDisk("h", 4096) // 1 x 4 KB block budget

			f, err := os.OpenFile("/m", os.O_CREATE|os.O_RDWR, 0o644)
			mustOK(t, "create", err)
			defer f.Close()
			mustOK(t, "truncate", f.Truncate(16<<10))

			ro, err := syscall.Mmap(int(f.Fd()), 0, 16<<10, syscall.PROT_READ, syscall.MAP_SHARED)
			mustOK(t, "mmap ro", err)
			defer syscall.Munmap(ro)
			for off := 0; off < len(ro); off += 4096 {
				if ro[off] != 0 {
					t.Fatalf("ro[%d] = %d, want 0", off, ro[off])
				}
			}
			if _, blks := fstatBlocks(t, f); blks != 0 {
				t.Fatalf("blocks after PROT_READ hole reads = %d, want 0", blks)
			}

			rw, err := syscall.Mmap(int(f.Fd()), 0, 16<<10, syscall.PROT_READ, syscall.MAP_SHARED)
			mustOK(t, "mmap upgradeable", err)
			defer syscall.Munmap(rw)
			mustOK(t, "mprotect rw", syscall.Mprotect(rw[4096:8192], syscall.PROT_READ|syscall.PROT_WRITE))
			// Reading the writable hole without writing must still charge 0 blocks
			// even if the host kernel faults a zeroed shmem page.
			if rw[4096] != 0 {
				t.Fatalf("rw[4096] = %d, want 0", rw[4096])
			}
			if _, blks := fstatBlocks(t, f); blks != 0 {
				t.Fatalf("blocks after clean rw hole read = %d, want 0", blks)
			}

			// Dirtying page 1 through the mapping charges the 1 free block.
			rw[4096] = 0x5a
			if _, blks := fstatBlocks(t, f); blks != 8 {
				t.Fatalf("blocks after mmap write = %d, want 8", blks)
			}
			// Disk is now full: pwrite into hole page 2 fails ENOSPC, while
			// pwrite into mmap-allocated page 1 succeeds.
			if n, err := f.WriteAt([]byte("x"), 8192); n != 0 || !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("hole pwrite after mmap fill: got (%d, %v), want (0, ENOSPC)", n, err)
			}
			_, err = f.WriteAt([]byte("ok"), 4100)
			mustOK(t, "in-place pwrite on mmap-allocated page", err)
		})
	})
}

// TestDSTFallocateHoleChargeAndDurability verifies that fallocate(2) mode 0 on
// a sparse file charges only unallocated 4 KB hole pages against LimitDisk
// (refusing all-or-nothing on ENOSPC), allows subsequent writes into zero-filled
// preallocated blocks without extra block charges, and preserves st_blocks across
// Sync + CrashHost.
func TestDSTFallocateHoleChargeAndDurability(t *testing.T) {
	simulation.Run(1, func() {
		onHost("h", func() {
			f, err := os.OpenFile("/w", os.O_CREATE|os.O_RDWR, 0o644)
			mustOK(t, "create", err)
			mustOK(t, "sparse truncate", f.Truncate(16<<10))
			_, err = f.WriteAt([]byte("x"), 4096) // allocate page 1 only
			mustOK(t, "write page 1", err)

			simulation.LimitDisk("h", 3*4096) // 3 blocks total => 2 free blocks left

			// [0, 16 KiB) has 3 unallocated hole pages (0, 2, 3) > 2 free blocks.
			wantErrno(t, "fallocate 3 holes with 2 free", falloc(f, 0, 0, 16<<10), syscall.ENOSPC)
			if _, blks := fstatBlocks(t, f); blks != 8 {
				t.Fatalf("blocks after refused fallocate = %d, want 8", blks)
			}

			// [0, 12 KiB) covers pages 0, 1, 2: page 1 is already allocated, so
			// only 2 hole pages are needed — fits exactly.
			mustOK(t, "fallocate 2 holes + 1 allocated", falloc(f, 0, 0, 12<<10))
			if _, blks := fstatBlocks(t, f); blks != 24 {
				t.Fatalf("blocks after fallocate = %d, want 24", blks)
			}

			// Page 3 is still a hole on a full disk -> ENOSPC; page 2 is preallocated -> succeeds.
			if n, err := f.WriteAt([]byte("no"), 12<<10); n != 0 || !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("write unallocated page 3: got (%d, %v), want (0, ENOSPC)", n, err)
			}
			_, err = f.WriteAt([]byte("yes"), 8<<10)
			mustOK(t, "write preallocated page 2", err)

			// Page 0 remains all-zero preallocated space; Sync + CrashHost must
			// preserve all 3 allocated blocks (24 sectors) on reboot.
			mustOK(t, "sync file", f.Sync())
			mustOK(t, "close file", f.Close())
			dir, err := os.Open("/")
			mustOK(t, "open root", err)
			mustOK(t, "sync root", dir.Sync())
			mustOK(t, "close root", dir.Close())
		})
		simulation.CrashHost("h")
		onHost("h", func() {
			f, err := os.Open("/w")
			mustOK(t, "reopen after crash", err)
			defer f.Close()
			sz, blks := fstatBlocks(t, f)
			if sz != 16<<10 || blks != 24 {
				t.Fatalf("after CrashHost: size=%d blocks=%d, want %d, 24", sz, blks, 16<<10)
			}
		})
	})
}

// TestDSTFSSparseMultiHundredGiBDurableAndCrash verifies that syncing,
// failing/healing writeback, corrupting, and crash-restoring (both untorn and
// CrashTear) a near-limit sparse file (100 GiB on wide 64-bit arches) stays
// sparse, shares unchanged 4 KB durable pages copy-on-write, and replays
// deterministically without densification.
func TestDSTFSSparseMultiHundredGiBDurableAndCrash(t *testing.T) {
	if ^uint(0)>>63 == 0 {
		t.Skip("large sparse memfd test requires 64-bit address space")
	}
	sparseSize := min(int64(100)<<30, os.DSTSMaxBytes())
	tailOff := sparseSize - 4096
	tailPg := tailOff / 4096

	t.Run("COWAndFsyncgateAndRot", func(t *testing.T) {
		simulation.Run(7, func() {
			onHost("h", func() {
				f, err := os.OpenFile("/big", os.O_CREATE|os.O_RDWR, 0o644)
				mustOK(t, "create", err)
				mustOK(t, "truncate sparse", f.Truncate(sparseSize))
				_, err = f.WriteAt([]byte("head-v1"), 0)
				mustOK(t, "write head", err)
				_, err = f.WriteAt([]byte("tail-v1"), tailOff)
				mustOK(t, "write tail", err)
				mustOK(t, "sync v1", f.Sync())
				dir, err := os.Open("/")
				mustOK(t, "open root", err)
				mustOK(t, "sync root", dir.Sync())
				mustOK(t, "close root", dir.Close())

				sz, ssz, allocPgs, durPgs, ok := os.DSTFSNodeSparseStats("/big")
				if !ok || sz != sparseSize || ssz != sparseSize || allocPgs != 2 || durPgs != 2 {
					t.Fatalf("sparse stats = (%d, %d, %d, %d, %v), want (%d, %d, 2, 2, true)",
						sz, ssz, allocPgs, durPgs, ok, sparseSize, sparseSize)
				}
				headPtr1, _ := os.DSTFSNodeDurablePagePtr("/big", 0)
				tailPtr1, _ := os.DSTFSNodeDurablePagePtr("/big", tailPg)
				if headPtr1 == 0 || tailPtr1 == 0 {
					t.Fatalf("expected non-zero durable page pointers, got %x, %x", headPtr1, tailPtr1)
				}

				// Overwrite page 0 only: page tailPg must share the exact same COW buffer.
				_, err = f.WriteAt([]byte("head-v2"), 0)
				mustOK(t, "write head v2", err)
				mustOK(t, "sync v2", f.Sync())
				headPtr2, _ := os.DSTFSNodeDurablePagePtr("/big", 0)
				tailPtr2, _ := os.DSTFSNodeDurablePagePtr("/big", tailPg)
				if headPtr2 == headPtr1 {
					t.Fatalf("modified page 0 reused old buffer %x", headPtr2)
				}
				if tailPtr2 != tailPtr1 {
					t.Fatalf("unchanged far page reallocated: got %x, want %x", tailPtr2, tailPtr1)
				}

				// FailWriteback on a 100 GiB sparse file must record fsyncgate in O(1).
				simulation.FailWriteback("h")
				_, err = f.WriteAt([]byte("dropped-mid"), 8192)
				mustOK(t, "write mid under FailWriteback", err)
				wantErrno(t, "sync under FailWriteback", f.Sync(), syscall.EIO)
				simulation.HealWriteback("h")
				mustOK(t, "retried sync after heal", f.Sync())
				_, _, _, durPgs, _ = os.DSTFSNodeSparseStats("/big")
				if durPgs != 2 {
					t.Fatalf("retried sync committed fsyncgate-dropped page: durPgs=%d, want 2", durPgs)
				}
				// Rewriting page 2 redirties it so the next sync commits it while keeping tailPtr1.
				_, err = f.WriteAt([]byte("redirtied-mid"), 8192)
				mustOK(t, "rewrite mid", err)
				mustOK(t, "sync after rewrite", f.Sync())
				_, _, _, durPgs, _ = os.DSTFSNodeSparseStats("/big")
				tailPtr3, _ := os.DSTFSNodeDurablePagePtr("/big", tailPg)
				if durPgs != 3 || tailPtr3 != tailPtr1 {
					t.Fatalf("after redirtied sync: durPgs=%d tailPtr=%x, want 3, %x", durPgs, tailPtr3, tailPtr1)
				}
				mustOK(t, "close", f.Close())

				simulation.CorruptFile("h", "/big")
			})
			simulation.CrashHost("h")
			onHost("h", func() {
				f, err := os.Open("/big")
				mustOK(t, "reopen after crash", err)
				defer f.Close()
				page0 := make([]byte, 4096)
				page2 := make([]byte, 4096)
				pageTail := make([]byte, 4096)
				_, err = f.ReadAt(page0, 0)
				mustOK(t, "read page 0", err)
				_, err = f.ReadAt(page2, 8192)
				mustOK(t, "read page 2", err)
				_, err = f.ReadAt(pageTail, tailOff)
				mustOK(t, "read tail page", err)

				var want0, want2, wantTail [4096]byte
				copy(want0[:], "head-v2")
				copy(want2[:], "redirtied-mid")
				copy(wantTail[:], "tail-v1")
				diffBits := 0
				for i := range 4096 {
					diffBits += bits.OnesCount8(page0[i] ^ want0[i])
					diffBits += bits.OnesCount8(page2[i] ^ want2[i])
					diffBits += bits.OnesCount8(pageTail[i] ^ wantTail[i])
				}

				// CorruptFile flips exactly 1 bit uniformly across [0, sparseSize):
				// either it hit one of the 3 non-zero pages (diffBits == 1, 3 pages
				// materialized) or it flipped 1 bit in a zero hole page (diffBits == 0
				// on those 3 pages, 4 pages materialized).
				sz, _, allocPgs, durPgs, ok := os.DSTFSNodeSparseStats("/big")
				validHitData := diffBits == 1 && allocPgs == 3 && durPgs == 3
				validHitHole := diffBits == 0 && allocPgs == 4 && durPgs == 4
				if !ok || sz != sparseSize || (!validHitData && !validHitHole) {
					t.Fatalf("post-crash rot/sparse invariant violated: sz=%d alloc=%d dur=%d diffBits=%d ok=%v",
						sz, allocPgs, durPgs, diffBits, ok)
				}
			})
		})
	})

	t.Run("CrashTearReplaySparse", func(t *testing.T) {
		isTornPrefix := func(got, cur, dur []byte) bool {
			for k := 0; k <= len(got); k++ {
				if bytes.Equal(got[:k], cur[:k]) && bytes.Equal(got[k:], dur[k:]) {
					return true
				}
			}
			return false
		}
		runOnce := func(seed uint64) (int64, []byte, []byte) {
			var gotSize int64
			head := make([]byte, 8)
			tail := make([]byte, 8)
			simulation.RunWith(seed, simulation.Options{CrashTear: true}, func() {
				onHost("h", func() {
					f, err := os.OpenFile("/tear", os.O_CREATE|os.O_RDWR, 0o644)
					mustOK(t, "create", err)
					mustOK(t, "truncate", f.Truncate(sparseSize/2))
					_, err = f.WriteAt([]byte("base0000"), 0)
					mustOK(t, "write base", err)
					mustOK(t, "sync base", f.Sync())
					dir, err := os.Open("/")
					mustOK(t, "open root", err)
					mustOK(t, "sync root", dir.Sync())
					mustOK(t, "close root", dir.Close())

					// Unsynced sparse growth + dirty pages at both ends.
					mustOK(t, "grow sparse", f.Truncate(sparseSize))
					_, err = f.WriteAt([]byte("NEWHEAD!"), 0)
					mustOK(t, "write dirty head", err)
					_, err = f.WriteAt([]byte("NEWTAIL!"), tailOff)
					mustOK(t, "write dirty tail", err)
					mustOK(t, "close", f.Close())
				})
				simulation.CrashHost("h")
				onHost("h", func() {
					f, err := os.Open("/tear")
					mustOK(t, "reopen", err)
					defer f.Close()
					st, err := f.Stat()
					mustOK(t, "stat", err)
					gotSize = st.Size()
					if gotSize < sparseSize/2 || gotSize > sparseSize || gotSize%4096 != 0 {
						t.Fatalf("invalid torn size %d (want page-aligned in [%d, %d])", gotSize, sparseSize/2, sparseSize)
					}
					_, err = f.ReadAt(head, 0)
					mustOK(t, "read torn head", err)
					if !isTornPrefix(head, []byte("NEWHEAD!"), []byte("base0000")) {
						t.Fatalf("invalid torn head %q", head)
					}
					if gotSize == sparseSize {
						_, err = f.ReadAt(tail, tailOff)
						mustOK(t, "read torn tail", err)
						if !isTornPrefix(tail, []byte("NEWTAIL!"), make([]byte, 8)) {
							t.Fatalf("invalid torn tail %q", tail)
						}
					}
					_, _, allocPgs, durPgs, ok := os.DSTFSNodeSparseStats("/tear")
					if !ok || allocPgs < 1 || allocPgs > 2 || durPgs < 1 || durPgs > 2 {
						t.Fatalf("torn sparse stats = (alloc=%d, dur=%d, ok=%v), want in [1, 2]", allocPgs, durPgs, ok)
					}
				})
			})
			return gotSize, head, tail
		}
		for seed := uint64(1); seed <= 8; seed++ {
			sz1, h1, t1 := runOnce(seed)
			sz2, h2, t2 := runOnce(seed)
			if sz1 != sz2 || !bytes.Equal(h1, h2) || !bytes.Equal(t1, t2) {
				t.Fatalf("seed %d CrashTear sparse replay diverged: (%d,%q,%q) vs (%d,%q,%q)", seed, sz1, h1, t1, sz2, h2, t2)
			}
		}
	})
}

// TestDSTFSPageCacheRegionAndMemfdReclamation pins node backing reclamation
// across aggregate sparse file churn (> 1.5x the 1 TiB mapping region) and all
// four retention guards (open handles, live mappings, direct durable parent
// links, and deep unlinked directory trees).
func TestDSTFSPageCacheRegionAndMemfdReclamation(t *testing.T) {
	if ^uint(0)>>63 == 0 {
		t.Skip("region reclamation test requires 64-bit page-cache region")
	}

	t.Run("AggregateRegionReuse", func(t *testing.T) {
		chunkSize := min(int64(64)<<30, os.DSTSMaxBytes()/2)
		simulation.Run(1, func() {
			onHost("h", func() {
				// 24 x 64 GiB = 1.5 TiB > 1 TiB mapping region on wide arches:
				// would exhaust dstMapRegion without retired-view reclamation.
				for i := range 24 {
					f, err := os.OpenFile("/ephemeral", os.O_CREATE|os.O_RDWR, 0o644)
					mustOK(t, "create", err)
					mustOK(t, "truncate", f.Truncate(chunkSize))
					_, err = f.WriteAt([]byte{byte(i + 1)}, chunkSize-4096)
					mustOK(t, "write", err)
					mustOK(t, "close", f.Close())
					mustOK(t, "remove", os.Remove("/ephemeral"))
					if got := os.DSTBackedNodeCount(); got != 0 {
						t.Fatalf("iter %d: backed nodes = %d after close+unlink, want 0", i, got)
					}
				}
			})
		})
	})

	t.Run("RetentionGuardsAndDeepSubtreeCascade", func(t *testing.T) {
		simulation.Run(2, func() {
			onHost("h", func() {
				// 1. Unlinked-but-open retains backing until Close.
				f, err := os.OpenFile("/u_open", os.O_CREATE|os.O_RDWR, 0o644)
				mustOK(t, "create u_open", err)
				_, err = f.Write([]byte("still-open"))
				mustOK(t, "write u_open", err)
				mustOK(t, "remove u_open", os.Remove("/u_open"))
				if got := os.DSTBackedNodeCount(); got != 1 {
					t.Fatalf("unlinked-but-open backed = %d, want 1", got)
				}
				buf := make([]byte, 10)
				_, err = f.ReadAt(buf, 0)
				mustOK(t, "read unlinked-but-open", err)
				if string(buf) != "still-open" {
					t.Fatalf("got %q, want still-open", buf)
				}
				mustOK(t, "close u_open", f.Close())
				if got := os.DSTBackedNodeCount(); got != 0 {
					t.Fatalf("after close u_open backed = %d, want 0", got)
				}

				// 2. Unlinked-but-mapped retains backing until Munmap.
				mf, err := os.OpenFile("/u_map", os.O_CREATE|os.O_RDWR, 0o644)
				mustOK(t, "create u_map", err)
				mustOK(t, "truncate u_map", mf.Truncate(4096))
				mm, err := syscall.Mmap(int(mf.Fd()), 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
				mustOK(t, "mmap u_map", err)
				mm[0] = 0x7f
				mustOK(t, "close u_map fd", mf.Close())
				mustOK(t, "remove u_map", os.Remove("/u_map"))
				if got := os.DSTBackedNodeCount(); got != 1 {
					t.Fatalf("unlinked-but-mapped backed = %d, want 1", got)
				}
				if mm[0] != 0x7f {
					t.Fatalf("mm[0] = %x, want 0x7f", mm[0])
				}
				mustOK(t, "munmap u_map", syscall.Munmap(mm))
				if got := os.DSTBackedNodeCount(); got != 0 {
					t.Fatalf("after munmap backed = %d, want 0", got)
				}

				// 3. Deep durable directory tree (/d/sub/f): RemoveAll("/d")
				// without syncing "/" must keep f's backing alive so CrashHost
				// resurrects /d/sub/f; syncing "/" after RemoveAll cascades
				// durableRefs -> 0 and reclaims f immediately.
				mustOK(t, "mkdirall", os.MkdirAll("/d/sub", 0o755))
				df, err := os.OpenFile("/d/sub/f", os.O_CREATE|os.O_RDWR, 0o644)
				mustOK(t, "create deep f", err)
				_, err = df.Write([]byte("deep-durable"))
				mustOK(t, "write deep f", err)
				mustOK(t, "sync deep f", df.Sync())
				mustOK(t, "close deep f", df.Close())
				for _, p := range []string{"/d/sub", "/d", "/"} {
					d, err := os.Open(p)
					mustOK(t, "open "+p, err)
					mustOK(t, "sync "+p, d.Sync())
					mustOK(t, "close "+p, d.Close())
				}
				if got := os.DSTBackedNodeCount(); got != 1 {
					t.Fatalf("synced deep f backed = %d, want 1", got)
				}

				mustOK(t, "unsynced RemoveAll(/d)", os.RemoveAll("/d"))
				if got := os.DSTBackedNodeCount(); got != 1 {
					t.Fatalf("after unsynced RemoveAll(/d): backed = %d, want 1 (must survive for CrashHost)", got)
				}
			})
			simulation.CrashHost("h")
			onHost("h", func() {
				if got := os.DSTBackedNodeCount(); got != 1 {
					t.Fatalf("after CrashHost resurrection: backed = %d, want 1", got)
				}
				gotBytes, err := os.ReadFile("/d/sub/f")
				mustOK(t, "read resurrected /d/sub/f", err)
				if string(gotBytes) != "deep-durable" {
					t.Fatalf("resurrected /d/sub/f = %q, want deep-durable", gotBytes)
				}

				// Now remove /d AND sync "/": recursive durableRefs drop must reclaim /d/sub/f.
				mustOK(t, "RemoveAll(/d) second time", os.RemoveAll("/d"))
				rootDir, err := os.Open("/")
				mustOK(t, "open /", err)
				mustOK(t, "sync /", rootDir.Sync())
				mustOK(t, "close /", rootDir.Close())
				if got := os.DSTBackedNodeCount(); got != 0 {
					t.Fatalf("after RemoveAll(/d) + Sync(/): backed = %d, want 0", got)
				}
			})
		})
	})
}
