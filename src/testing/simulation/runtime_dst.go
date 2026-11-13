// Copyright 2026 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package simulation

import (
	"testing"
	_ "unsafe" // for go:linkname
)

//go:linkname dstActivate runtime.dstActivate
func dstActivate(seed uint64)

//go:linkname dstDeactivate runtime.dstDeactivate
func dstDeactivate()

//go:linkname dstSetAsyncPreemptOff runtime.dstSetAsyncPreemptOff
func dstSetAsyncPreemptOff(off bool) (old bool)

//go:linkname dstBuilt runtime.dstBuilt
func dstBuilt() bool

//go:linkname dstSetSchedStrategy runtime.dstSetSchedStrategy
func dstSetSchedStrategy(kind uint8, depth, steps int32)

//go:linkname dstExploreInit runtime.dstExploreInit
func dstExploreInit(maxDecisions, maxEnabledTotal, maxEdges, maxAccesses int)

//go:linkname dstSetSchedule runtime.dstSetSchedule
func dstSetSchedule(prefix []uint64)

//go:linkname dstSetAccessForce runtime.dstSetAccessForce
func dstSetAccessForce(seq, count []uint64, pc []uintptr)

//go:linkname dstTraceLenFP runtime.dstTraceLenFP
func dstTraceLenFP() int

//go:linkname dstSchedForeignSeenFP runtime.dstSchedForeignSeenFP
func dstSchedForeignSeenFP() bool

//go:linkname dstTraceChosenFP runtime.dstTraceChosenFP
func dstTraceChosenFP(i int) uint64

//go:linkname dstTraceAccessFP runtime.dstTraceAccessFP
func dstTraceAccessFP(i int) (addr uintptr, write bool)

//go:linkname dstTraceEnabledFP runtime.dstTraceEnabledFP
func dstTraceEnabledFP(i int) []uint64

//go:linkname dstTraceOverflowFP runtime.dstTraceOverflowFP
func dstTraceOverflowFP() bool

//go:linkname dstTraceEnabOverflowFP runtime.dstTraceEnabOverflowFP
func dstTraceEnabOverflowFP() bool

//go:linkname dstExplorePanicFP runtime.dstExplorePanicFP
func dstExplorePanicFP() (any, bool)

//go:linkname dstExploreDeadlockFP runtime.dstExploreDeadlockFP
func dstExploreDeadlockFP() string

//go:linkname dstEdgeLenFP runtime.dstEdgeLenFP
func dstEdgeLenFP() int

//go:linkname dstEnsureSeqSelfFP runtime.dstEnsureSeqSelfFP
func dstEnsureSeqSelfFP() uint64

//go:linkname dstEdgeAtFP runtime.dstEdgeAtFP
func dstEdgeAtFP(i int) (from, to uint64, step, acc int)

//go:linkname dstEdgeOrderFP runtime.dstEdgeOrderFP
func dstEdgeOrderFP(i int) int

//go:linkname dstEdgeOverflowFP runtime.dstEdgeOverflowFP
func dstEdgeOverflowFP() bool

//go:linkname dstSyncEventLenFP runtime.dstSyncEventLenFP
func dstSyncEventLenFP() int

//go:linkname dstSyncEventAtFP runtime.dstSyncEventAtFP
func dstSyncEventAtFP(i int) (kind uint8, id, aux uintptr, seq uint64, step, acc, order int)

//go:linkname dstAccLogLenFP runtime.dstAccLogLenFP
func dstAccLogLenFP() int

//go:linkname dstAccLogAtFP runtime.dstAccLogAtFP
func dstAccLogAtFP(i int) (seq uint64, addr uintptr, size uintptr, pc uintptr, count uint64, write bool, step int)

//go:linkname dstAccLogOverflowFP runtime.dstAccLogOverflowFP
func dstAccLogOverflowFP() bool

//go:linkname dstRaceEnabledFP runtime.dstRaceEnabledFP
func dstRaceEnabledFP() bool

//go:linkname dstLevel2EventsFP runtime.dstLevel2EventsFP
func dstLevel2EventsFP() uint64

//go:linkname dstMultiChoicePointsFP runtime.dstMultiChoicePointsFP
func dstMultiChoicePointsFP() uint64

//go:linkname dstSyncBlindBlocksFP runtime.dstSyncBlindBlocksFP
func dstSyncBlindBlocksFP() uint64

//go:linkname dstSyncAnnouncesFP runtime.dstSyncAnnouncesFP
func dstSyncAnnouncesFP() uint64

//go:linkname dstScheduleAbortedFP runtime.dstScheduleAbortedFP
func dstScheduleAbortedFP() bool

//go:linkname dstScheduleAbortStepFP runtime.dstScheduleAbortStepFP
func dstScheduleAbortStepFP() int32

//go:linkname dstExplorePanicStepFP runtime.dstExplorePanicStepFP
func dstExplorePanicStepFP() int32

//go:linkname dstSyncEventOverflowFP runtime.dstSyncEventOverflowFP
func dstSyncEventOverflowFP() bool

//go:linkname dstSetSimEnv runtime.dstSetSimEnv
func dstSetSimEnv(hostname string, pid, numcpu, pidmax int)

//go:linkname dstClearSimEnv runtime.dstClearSimEnv
func dstClearSimEnv()

//go:linkname dstEnvDispatchLock runtime.dstEnvDispatchLock
func dstEnvDispatchLock()

//go:linkname dstEnvDispatchUnlock runtime.dstEnvDispatchUnlock
func dstEnvDispatchUnlock()

//go:linkname dstSetMemLimit runtime.dstSetMemLimit
func dstSetMemLimit(limit int64)

//go:linkname dstSetNetCrossHostLatency runtime.dstSetNetCrossHostLatency
func dstSetNetCrossHostLatency(ns int64)

//go:linkname dstSetNetCrossHostJitter runtime.dstSetNetCrossHostJitter
func dstSetNetCrossHostJitter(ns int64)

//go:linkname dstSetNetCrossHostBandwidth runtime.dstSetNetCrossHostBandwidth
func dstSetNetCrossHostBandwidth(bytesPerSec int64)

//go:linkname dstSetNetSendBuffer runtime.dstSetNetSendBuffer
func dstSetNetSendBuffer(bytes int64)

//go:linkname dstSetNetRetransmitTimeout runtime.dstSetNetRetransmitTimeout
func dstSetNetRetransmitTimeout(ns int64)

//go:linkname dstSetWedgeLimits runtime.dstSetWedgeLimits
func dstSetWedgeLimits(decisions, wallNs int64)

//go:linkname testingSimulationTest testing/simulation.testingSimulationTest
func testingSimulationTest(t *testing.T, f func(*testing.T)) bool

//go:linkname testingSimulationCleanupStarted testing/simulation.testingSimulationCleanupStarted
func testingSimulationCleanupStarted(t *testing.T) bool

//go:linkname dstGOMAXPROCSAuto runtime.dstGOMAXPROCSAutoFP
func dstGOMAXPROCSAuto() bool

//go:linkname dstSetNode runtime.dstSetNode
func dstSetNode(host, proc uint32) (oldHost, oldProc uint32)

//go:linkname dstPushHostScope runtime.dstPushHostScope
func dstPushHostScope(host, proc uint32) (oldHost, oldProc uint32)

//go:linkname dstPopHostScope runtime.dstPopHostScope
func dstPopHostScope(oldHost, oldProc uint32)

//go:linkname dstCurrentNode runtime.dstCurrentNode
func dstCurrentNode() (host, proc uint32)

//go:linkname dstNetEpoch runtime.dstNetEpoch
func dstNetEpoch() uint64

//go:linkname dstReestablishHostClock runtime.dstReestablishHostClock
func dstReestablishHostClock(host uint32, offset, ppb int64) bool

//go:linkname dstHostSeededClockOffset runtime.dstHostSeededClockOffset
func dstHostSeededClockOffset(hostid uint32, bound int64) int64

//go:linkname dstHostSeededDriftPPB runtime.dstHostSeededDriftPPB
func dstHostSeededDriftPPB(hostid uint32, maxPPB int64) int64

//go:linkname dstSetHostIdent runtime.dstSetHostIdent
func dstSetHostIdent(host uint32, hostname string, numcpu int)

//go:linkname dstBootHost runtime.dstBootHost
func dstBootHost(host uint32)

//go:linkname dstAllocPid runtime.dstAllocPid
func dstAllocPid() int32

//go:linkname dstCheckPidAvailable runtime.dstCheckPidAvailable
func dstCheckPidAvailable()

//go:linkname dstSetProcessPid runtime.dstSetProcessPid
func dstSetProcessPid(pid int32) (old int32)

//go:linkname dstRegisterPid runtime.dstRegisterPid
func dstRegisterPid(pid int32, ns uint32)

//go:linkname dstUnregisterPid runtime.dstUnregisterPid
func dstUnregisterPid(pid int32)

//go:linkname dstCrashProcessPid runtime.dstCrashProcessPid
func dstCrashProcessPid(pid int32)

//go:linkname dstStopProcessThreads runtime.dstStopProcessThreads
func dstStopProcessThreads(pid int32)

//go:linkname dstProcessTeardown runtime.dstProcessTeardown
func dstProcessTeardown(proc uint32)

//go:linkname dstProcessStateTeardown runtime.dstProcessStateTeardown
func dstProcessStateTeardown(proc uint32)

//go:linkname dstSetMappingFaultTeardownHook runtime.dstSetMappingFaultTeardownHook
func dstSetMappingFaultTeardownHook(fn func(proc uint32))

//go:linkname dstSetHostDeclaredHook runtime.dstSetHostDeclaredHook
func dstSetHostDeclaredHook(fn func(host uint32) bool)

//go:linkname dstSelfCrashed runtime.dstSelfCrashed
func dstSelfCrashed() bool

//go:linkname dstParkCrashedSelf runtime.dstParkCrashedSelf
func dstParkCrashedSelf()

//go:linkname dstPidAliveSim runtime.dstPidAlive
func dstPidAliveSim(pid int32) bool

//go:linkname dstPidOwnsBubbleMain runtime.dstPidOwnsBubbleMain
func dstPidOwnsBubbleMain(pid int32) bool

//go:linkname dstHostOwnsBubbleMain runtime.dstHostOwnsBubbleMain
func dstHostOwnsBubbleMain(host uint32) bool

//go:linkname dstMarkHostGoroutinesCrashed runtime.dstMarkHostGoroutinesCrashed
func dstMarkHostGoroutinesCrashed(host uint32)

//go:linkname dstProcAllocEnsure runtime.dstProcAllocEnsure
func dstProcAllocEnsure(procid uint32)

//go:linkname dstProcAllocBytes runtime.dstProcAllocBytes
func dstProcAllocBytes(procid uint32) int64

//go:linkname dstNetPartitionOp runtime.dstNetPartitionOp
func dstNetPartitionOp(op, a, b uint32)

//go:linkname dstNetHostDead runtime.dstNetHostDead
func dstNetHostDead(host uint32) bool

//go:linkname dstInSimBubble runtime.dstInSimBubble
func dstInSimBubble() bool

//go:linkname dstStepHostClock runtime.dstStepHostClock
func dstStepHostClock(host uint32, delta int64) bool

//go:linkname dstDriftHostClock runtime.dstDriftHostClock
func dstDriftHostClock(host uint32, ppb int64)

//go:linkname dstDiskFaultOp runtime.dstDiskFaultOp
func dstDiskFaultOp(op, host uint32, arg int64, name string)
