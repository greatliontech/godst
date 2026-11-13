// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build dst

package runtime

// A waitReason explains why a goroutine has been stopped.
// See gopark. Do not re-use waitReasons, add new ones.
type waitReason uint8

const (
	waitReasonZero                  waitReason = iota // ""
	waitReasonGCAssistMarking                         // "GC assist marking"
	waitReasonIOWait                                  // "IO wait"
	waitReasonDumpingHeap                             // "dumping heap"
	waitReasonGarbageCollection                       // "garbage collection"
	waitReasonGarbageCollectionScan                   // "garbage collection scan"
	waitReasonPanicWait                               // "panicwait"
	waitReasonGCAssistWait                            // "GC assist wait"
	waitReasonGCSweepWait                             // "GC sweep wait"
	waitReasonGCScavengeWait                          // "GC scavenge wait"
	waitReasonFinalizerWait                           // "finalizer wait"
	waitReasonForceGCIdle                             // "force gc (idle)"
	waitReasonUpdateGOMAXPROCSIdle                    // "GOMAXPROCS updater (idle)"
	waitReasonSemacquire                              // "semacquire"
	waitReasonSleep                                   // "sleep"
	waitReasonChanReceiveNilChan                      // "chan receive (nil chan)"
	waitReasonChanSendNilChan                         // "chan send (nil chan)"
	waitReasonSelectNoCases                           // "select (no cases)"
	waitReasonSelect                                  // "select"
	waitReasonChanReceive                             // "chan receive"
	waitReasonChanSend                                // "chan send"
	waitReasonSyncCondWait                            // "sync.Cond.Wait"
	waitReasonSyncMutexLock                           // "sync.Mutex.Lock"
	waitReasonSyncRWMutexRLock                        // "sync.RWMutex.RLock"
	waitReasonSyncRWMutexLock                         // "sync.RWMutex.Lock"
	waitReasonSyncWaitGroupWait                       // "sync.WaitGroup.Wait"
	waitReasonTraceReaderBlocked                      // "trace reader (blocked)"
	waitReasonWaitForGCCycle                          // "wait for GC cycle"
	waitReasonGCWorkerIdle                            // "GC worker (idle)"
	waitReasonGCWorkerActive                          // "GC worker (active)"
	waitReasonPreempted                               // "preempted"
	waitReasonDebugCall                               // "debug call"
	waitReasonGCMarkTermination                       // "GC mark termination"
	waitReasonStoppingTheWorld                        // "stopping the world"
	waitReasonFlushProcCaches                         // "flushing proc caches"
	waitReasonTraceGoroutineStatus                    // "trace goroutine status"
	waitReasonTraceProcStatus                         // "trace proc status"
	waitReasonPageTraceFlush                          // "page trace flush"
	waitReasonCoroutine                               // "coroutine"
	waitReasonGCWeakToStrongWait                      // "GC weak to strong wait"
	waitReasonSynctestRun                             // "synctest.Run"
	waitReasonSynctestWait                            // "synctest.Wait"
	waitReasonSynctestChanReceive                     // "chan receive (durable)"
	waitReasonSynctestChanSend                        // "chan send (durable)"
	waitReasonSynctestSelect                          // "select (durable)"
	waitReasonSynctestWaitGroupWait                   // "sync.WaitGroup.Wait (durable)"
	waitReasonCleanupWait                             // "cleanup wait"
	waitReasonSynctestGCDrain                         // "synctest GC drain"
	waitReasonDSTProcessCrashed                       // "dst process crashed"
)

var waitReasonStrings = [...]string{
	waitReasonZero:                  "",
	waitReasonGCAssistMarking:       "GC assist marking",
	waitReasonIOWait:                "IO wait",
	waitReasonChanReceiveNilChan:    "chan receive (nil chan)",
	waitReasonChanSendNilChan:       "chan send (nil chan)",
	waitReasonDumpingHeap:           "dumping heap",
	waitReasonGarbageCollection:     "garbage collection",
	waitReasonGarbageCollectionScan: "garbage collection scan",
	waitReasonPanicWait:             "panicwait",
	waitReasonSelect:                "select",
	waitReasonSelectNoCases:         "select (no cases)",
	waitReasonGCAssistWait:          "GC assist wait",
	waitReasonGCSweepWait:           "GC sweep wait",
	waitReasonGCScavengeWait:        "GC scavenge wait",
	waitReasonChanReceive:           "chan receive",
	waitReasonChanSend:              "chan send",
	waitReasonFinalizerWait:         "finalizer wait",
	waitReasonForceGCIdle:           "force gc (idle)",
	waitReasonUpdateGOMAXPROCSIdle:  "GOMAXPROCS updater (idle)",
	waitReasonSemacquire:            "semacquire",
	waitReasonSleep:                 "sleep",
	waitReasonSyncCondWait:          "sync.Cond.Wait",
	waitReasonSyncMutexLock:         "sync.Mutex.Lock",
	waitReasonSyncRWMutexRLock:      "sync.RWMutex.RLock",
	waitReasonSyncRWMutexLock:       "sync.RWMutex.Lock",
	waitReasonSyncWaitGroupWait:     "sync.WaitGroup.Wait",
	waitReasonTraceReaderBlocked:    "trace reader (blocked)",
	waitReasonWaitForGCCycle:        "wait for GC cycle",
	waitReasonGCWorkerIdle:          "GC worker (idle)",
	waitReasonGCWorkerActive:        "GC worker (active)",
	waitReasonPreempted:             "preempted",
	waitReasonDebugCall:             "debug call",
	waitReasonGCMarkTermination:     "GC mark termination",
	waitReasonStoppingTheWorld:      "stopping the world",
	waitReasonFlushProcCaches:       "flushing proc caches",
	waitReasonTraceGoroutineStatus:  "trace goroutine status",
	waitReasonTraceProcStatus:       "trace proc status",
	waitReasonPageTraceFlush:        "page trace flush",
	waitReasonCoroutine:             "coroutine",
	waitReasonGCWeakToStrongWait:    "GC weak to strong wait",
	waitReasonSynctestRun:           "synctest.Run",
	waitReasonSynctestWait:          "synctest.Wait",
	waitReasonSynctestChanReceive:   "chan receive (durable)",
	waitReasonSynctestChanSend:      "chan send (durable)",
	waitReasonSynctestSelect:        "select (durable)",
	waitReasonSynctestWaitGroupWait: "sync.WaitGroup.Wait (durable)",
	waitReasonCleanupWait:           "cleanup wait",
	waitReasonSynctestGCDrain:       "synctest GC drain",
	waitReasonDSTProcessCrashed:     "dst process crashed",
}

func (w waitReason) String() string {
	if w < 0 || w >= waitReason(len(waitReasonStrings)) {
		return "unknown wait reason"
	}
	return waitReasonStrings[w]
}

// isMutexWait returns true if the goroutine is blocked because of
// sync.Mutex.Lock or sync.RWMutex.[R]Lock.
//
//go:nosplit
func (w waitReason) isMutexWait() bool {
	return w == waitReasonSyncMutexLock ||
		w == waitReasonSyncRWMutexRLock ||
		w == waitReasonSyncRWMutexLock
}

// isSyncWait returns true if the goroutine is blocked because of
// sync library primitive operations.
//
//go:nosplit
func (w waitReason) isSyncWait() bool {
	return waitReasonSyncCondWait <= w && w <= waitReasonSyncWaitGroupWait
}

// isChanWait is true if the goroutine is blocked because of non-nil
// channel operations or a select statement with at least one case.
//
//go:nosplit
func (w waitReason) isChanWait() bool {
	return w == waitReasonSelect ||
		w == waitReasonChanReceive ||
		w == waitReasonChanSend
}

func (w waitReason) isWaitingForSuspendG() bool {
	return isWaitingForSuspendG[w]
}

// isWaitingForSuspendG indicates that a goroutine is only entering _Gwaiting and
// setting a waitReason because it needs to be able to let the suspendG
// (used by the GC and the execution tracer) take ownership of its stack.
// The G is always actually executing on the system stack in these cases.
//
// TODO(mknyszek): Consider replacing this with a new dedicated G status.
var isWaitingForSuspendG = [len(waitReasonStrings)]bool{
	waitReasonStoppingTheWorld:      true,
	waitReasonGCMarkTermination:     true,
	waitReasonGarbageCollection:     true,
	waitReasonGarbageCollectionScan: true,
	waitReasonTraceGoroutineStatus:  true,
	waitReasonTraceProcStatus:       true,
	waitReasonPageTraceFlush:        true,
	waitReasonGCAssistMarking:       true,
	waitReasonGCWorkerActive:        true,
	waitReasonFlushProcCaches:       true,
}

func (w waitReason) isIdleInSynctest() bool {
	return isIdleInSynctest[w]
}

// isIdleInSynctest indicates that a goroutine is considered idle by synctest.Wait.
var isIdleInSynctest = [len(waitReasonStrings)]bool{
	waitReasonChanReceiveNilChan:    true,
	waitReasonChanSendNilChan:       true,
	waitReasonSelectNoCases:         true,
	waitReasonSleep:                 true,
	waitReasonSyncCondWait:          true,
	waitReasonSynctestWaitGroupWait: true,
	waitReasonCoroutine:             true,
	waitReasonSynctestRun:           true,
	waitReasonSynctestWait:          true,
	waitReasonSynctestChanReceive:   true,
	waitReasonSynctestChanSend:      true,
	waitReasonSynctestSelect:        true,
	// The DST finalizer-drain goroutine, when parked with no finalizers to run,
	// is durably idle: it is woken only by the synctest driver at a quiescence
	// point. Without this, an idle drain would keep running > 0 so the bubble
	// never quiesces and virtual time never advances (invariant DST-FIN-3).
	waitReasonSynctestGCDrain: true,
}
