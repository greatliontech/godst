// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !dst

package simulation

import (
	"sync"
	"testing"
)

type kernelMutex = sync.Mutex

func dstActivate(seed uint64)                                                       {}
func dstDeactivate()                                                                {}
func dstSetAsyncPreemptOff(off bool) (old bool)                                     { return false }
func dstBuilt() bool                                                                { return false }
func dstSetSchedStrategy(kind uint8, depth, steps int32)                            {}
func dstExploreInit(maxDecisions, maxEnabledTotal, maxEdges, maxAccesses int)       {}
func dstYieldPoint()                                                                {}
func dstSetSchedule(prefix []uint64)                                                {}
func dstSetAccessForce(seq, count []uint64, pc []uintptr)                           {}
func dstTraceLenFP() int                                                            { return 0 }
func dstSchedForeignSeenFP() bool                                                   { return false }
func dstTraceChosenFP(i int) uint64                                                 { return 0 }
func dstTraceAccessFP(i int) (addr uintptr, write bool)                             { return 0, false }
func dstTraceEnabledFP(i int) []uint64                                              { return nil }
func dstTraceOverflowFP() bool                                                      { return false }
func dstTraceEnabOverflowFP() bool                                                  { return false }
func dstExplorePanicFP() (any, bool)                                                { return nil, false }
func dstExploreDeadlockFP() string                                                  { return "" }
func dstEdgeLenFP() int                                                             { return 0 }
func dstEnsureSeqSelfFP() uint64                                                    { return 0 }
func dstEdgeAtFP(i int) (from, to uint64, step, acc int)                            { return 0, 0, 0, 0 }
func dstEdgeOrderFP(i int) int                                                      { return 0 }
func dstEdgeOverflowFP() bool                                                       { return false }
func dstSyncEventLenFP() int                                                        { return 0 }
func dstSyncEventAtFP(i int) (kind uint8, id, aux uintptr, seq uint64, step, acc, order int) {
	return 0, 0, 0, 0, 0, 0, 0
}
func dstAccLogLenFP() int { return 0 }
func dstAccLogAtFP(i int) (seq uint64, addr uintptr, size uintptr, pc uintptr, count uint64, write bool, step int) {
	return 0, 0, 0, 0, 0, false, 0
}
func dstAccLogOverflowFP() bool                                    { return false }
func dstRaceEnabledFP() bool                                       { return false }
func dstLevel2EventsFP() uint64                                    { return 0 }
func dstMultiChoicePointsFP() uint64                               { return 0 }
func dstSyncBlindBlocksFP() uint64                                 { return 0 }
func dstSyncAnnouncesFP() uint64                                   { return 0 }
func dstScheduleAbortedFP() bool                                   { return false }
func dstScheduleAbortStepFP() int32                                { return 0 }
func dstExplorePanicStepFP() int32                                 { return 0 }
func dstSyncEventOverflowFP() bool                                 { return false }
func dstSetSimEnv(hostname string, pid, numcpu, pidmax int)        {}
func dstClearSimEnv()                                              {}
func dstEnvDispatchLock()                                          {}
func dstEnvDispatchUnlock()                                        {}
func dstSetMemLimit(limit int64)                                   {}
func dstSetNetCrossHostLatency(ns int64)                           {}
func dstSetNetCrossHostJitter(ns int64)                            {}
func dstSetNetCrossHostBandwidth(bytesPerSec int64)                {}
func dstSetNetSendBuffer(bytes int64)                              {}
func dstSetNetRetransmitTimeout(ns int64)                          {}
func dstSetWedgeLimits(decisions, wallNs int64)                    {}
func testingSimulationTest(t *testing.T, f func(*testing.T)) bool  { return false }
func testingSimulationCleanupStarted(t *testing.T) bool            { return false }
func dstGOMAXPROCSAuto() bool                                      { return false }
func dstSetNode(host, proc uint32) (oldHost, oldProc uint32)       { return 0, 0 }
func dstPushHostScope(host, proc uint32) (oldHost, oldProc uint32) { return 0, 0 }
func dstPopHostScope(oldHost, oldProc uint32)                      {}
func dstCurrentNode() (host, proc uint32)                          { return 0, 0 }
func dstNetEpoch() uint64                                          { return 0 }
func dstReestablishHostClock(host uint32, offset, ppb int64) bool  { return false }
func dstHostSeededClockOffset(hostid uint32, bound int64) int64    { return 0 }
func dstHostSeededDriftPPB(hostid uint32, maxPPB int64) int64      { return 0 }
func dstSetHostIdent(host uint32, hostname string, numcpu int)     {}
func dstBootHost(host uint32)                                      {}
func dstAllocPid() int32                                           { return 0 }
func dstCheckPidAvailable()                                        {}
func dstSetProcessPid(pid int32) (old int32)                       { return 0 }
func dstRegisterPid(pid int32, ns uint32)                          {}
func dstUnregisterPid(pid int32)                                   {}
func dstCrashProcessPid(pid int32)                                 {}
func dstStopProcessThreads(pid int32)                              {}
func dstProcessTeardown(proc uint32)                               {}
func dstProcessHasResources(proc uint32) bool                      { return false }
func dstProcessStateTeardown(proc uint32)                          {}
func dstSetMappingFaultTeardownHook(fn func(proc uint32))          {}
func dstSetHostDeclaredHook(fn func(host uint32) bool)             {}
func dstSelfCrashed() bool                                         { return false }
func dstParkCrashedSelf()                                          {}
func dstPidAliveSim(pid int32) bool                                { return false }
func dstPidOwnsBubbleMain(pid int32) bool                          { return false }
func dstHostOwnsBubbleMain(host uint32) bool                       { return false }
func dstMarkHostGoroutinesCrashed(host uint32)                     {}
func dstProcAllocEnsure(procid uint32)                             {}
func dstProcAllocBytes(procid uint32) int64                        { return 0 }
func dstNetPartitionOp(op, a, b uint32)                            {}
func dstNetHostDead(host uint32) bool                              { return false }
func dstInSimBubble() bool                                         { return false }
func dstStepHostClock(host uint32, delta int64) bool               { return false }
func dstDriftHostClock(host uint32, ppb int64)                     {}
func dstDiskFaultOp(op, host uint32, arg int64, name string) {}
func dstSnapshotRunDiagnostics() (
	killMeta [][5]uint64,
	killSeqs []uint64,
	killGoPCs []uintptr,
	killStartPCs []uintptr,
	killStates []string,
	droppedKillEvents uint32,
	stdioMeta [][6]uint64,
	stdioData []byte,
	stdioTruncated uint64,
) {
	return
}
