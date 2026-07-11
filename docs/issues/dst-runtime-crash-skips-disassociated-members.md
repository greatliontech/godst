# DST crash scans skip temporarily disassociated simulation goroutines

Lands: when process and host crash victim scans key on sticky simulation
membership rather than the transient bubble pointer

## Gap

Severity H. `dstMarkProcessGoroutinesCrashed` and
`dstMarkHostGoroutinesCrashed` filter on `gp.bubble == dstSimBubble`.
GC entry and assist paths temporarily clear that field while retaining the
goroutine's sticky simulation membership. A crash during that window can mark
the pid dead and tear down its resources while skipping one of its threads;
when GC restores the bubble field, that thread resumes after process or host
death.

## Required outcome

Crash victim enumeration includes every sticky member of the active
simulation, including a goroutine temporarily disassociated for GC. A
regression holds a victim in that window, crashes its process and host in
separate arms, and proves no code or defer after the GC point executes.
