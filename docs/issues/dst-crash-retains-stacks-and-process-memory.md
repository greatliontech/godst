# Crashed goroutine stacks retain process memory as GC roots

Lands: when process crash makes the victim invocation's stack-reachable memory
collectible without resuming or unwinding its goroutines

## Gap

Severity H. `dstMarkProcessGoroutinesCrashed` permanently parks victim
goroutines by negating `dstPid`, but leaves their stacks in `allgs`. The GC root
scan does not exclude those goroutines, so every object reachable from an
abandoned stack remains live forever. Repeated crash and restart cycles can
retain one process generation's heap per cycle until the containing process
runs out of memory, despite the contract that process memory is gone.

## Required outcome

A crashed invocation contributes no live roots while its goroutines remain
unrunnable and do not execute defers. A restart stress test retains a large
object on each victim stack, crashes, forces GC, and proves memory does not
grow with the number of dead generations.
