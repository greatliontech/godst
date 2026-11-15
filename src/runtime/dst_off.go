// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package runtime

import "unsafe"

// dstBuild is false in a normal build: the map hash key is seeded from OS
// entropy (hash-flooding protection), testing/simulation.Run refuses to run,
// and every dstBuild-guarded branch is stripped during Unified IR construction
// with zero struct or text overhead. See dst_on.go and docs/dst/design.md.
const dstBuild = false

type dstG struct{}
type dstP struct{}
type dstTimer struct{}
type dstBubble struct{}

const (
	dstSyncRendezvousAux = ^uintptr(0)
	dstSyncEventRelease  = 1
	dstSyncEventAcquire  = 2
)

func dstActive() bool                                                          { return false }
func dstInSimBubble() bool                                                     { return false }
func dstFenceActive() bool                                                     { return false }
func dstCgoRefuse()                                                            {}
func dstCallbackWorkersBlocked() bool                                          { return false }
func dstIsCrashedG(*g) bool                                                    { return false }
func dstCanPreemptG(*g) bool                                                   { return true }
func dstDurableMutexWait(*g) bool                                              { return false }
func dstIsSynctestGCDrainG(*g) bool                                            { return false }
func dstSyncAcquire(unsafe.Pointer)                                            {}
func dstSyncObserve(unsafe.Pointer)                                            {}
func dstRecordSyncAcquireID(uintptr, uintptr)                                  {}
func dstRecordSyncReleaseID(uintptr, uintptr)                                  {}
func dstRecordSyncEventForGID(uint8, uintptr, uintptr, *g)                     {}
func dstMaybeRunGOMAXPROCSSTWHook()                                            {}
func dstSimNumCPUForCaller() (int, bool)                                       { return 0, false }
func dstMallocgcPost(unsafe.Pointer, uintptr, *_type)                          {}
func dstCleanupSlabMove(*cleanupBlock, uint32, *cleanupBlock, uint32, uint32)  {}
func dstBeforeForcedGC()                                                       {}
func dstAfterCycleCaptureForcedGC()                                            {}
func dstRefuseForeignForcedGC()                                                {}
func dstGCTriggerHeapTest() bool                                               { return false }
func dstUseStopTheWorldGC() bool                                               { return false }
func dstEnterGCDisassociate(*g) bool                                           { return false }
func dstEnterGCAssistDisassociate(*g) bool                                     { return false }
func dstExitGCDisassociate(*g, bool)                                           {}
func dstGCResetLive()                                                          {}
func dstNoteUpdateMaxProcsSTW()                                                {}
func dstSysmonUpdateMaxProcsAllowed() bool                                     { return true }
func dstNoteSysmonMaxProcsActiveSkip()                                         {}
func dstSysmonUpdateMaxProcsBeforeLock()                                       {}
func dstSysmonUpdateMaxProcsAfterLock()                                        {}
func dstNoteSysmonMaxProcsRecheckBail()                                        {}
func dstEnterSetGCPercent()                                                    {}
func dstExitSetGCPercent()                                                     {}
func dstRegisterSpecialStamp(unsafe.Pointer)                                   {}
func dstDiscardSpecialStampLocked(unsafe.Pointer)                              {}
func dstStageSpecialStamps(unsafe.Pointer)                                     {}
func dstExploreRecordUncaughtPanic(any) bool                                   { return false }
func dstExploreDropPanicDefers(*g)                                             {}
func dstGoready(*g)                                                            {}
func dstAllgadd()                                                              {}
func dstFindRunnable(*p) (*g, bool)                                            { return nil, false }
func dstGdestroy(*g)                                                           {}
func dstNewprocReadyEdge(*g, *g)                                               {}
func dstNewprocPostYield()                                                     {}
func dstNewproc1Init(*g, *g)                                                   {}
func dstWedgeSysmonCheck(int64)                                                {}
func dstRunqputOverflow(*p, *g, uint32, uint32) bool                           { return false }
func dstFixedSeed(*[32]byte)                                                   {}
func dstRand() (uint64, bool)                                                  { return 0, false }
func dstSelectPollOrder(*g, uint32) uint32                                     { return 0 }
func dstNoteSyncBlindBlock(waitReason)                                         {}
func dstNotifyListNotifyOne(*notifyList, uint32)                               {}
func dstClockCopyoutFault(*g) bool                                             { return false }
func dstMappingSigpanic(*g)                                                    {}
func dstSynctestMaybeWakeLocked(*synctestBubble)                               {}
func dstSynctestDeferCleanup(*g, *synctestBubble)                              {}
func dstSynctestClaimBubble(*g, *synctestBubble)                               {}
func dstSynctestSpawnDrain(*p, *g, uintptr, *synctestBubble)                   {}
func dstSynctestDrainAtQuiescence(*synctestBubble)                             {}
func dstSynctestStopGCDrain(*synctestBubble)                                   {}
func dstExploreRecordDeadlock(string, *synctestBubble) bool                    { return false }
func dstTimeNowWall(int64) (int64, int32)                                      { return 0, 0 }
func dstTimerModify(*timer, int64, int64) (int64, int64)                       { return 0, 0 }
func dstTimerRand() uint32                                                     { return 0 }
func dstTimerConsumeWhenShift(*timer, int64) int64                             { return 0 }
func dstKernelLiftGoStartIgnoreSync(*g) bool                                   { return false }
