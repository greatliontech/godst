# DST fake-timer epoch rollover can erase a concurrent registration

Lands: when fake-timer epoch rollover and registration cannot lose an armed
timer under the multi-P white-box activation path

## Gap

Severity M. `dstFakeTimersRoll` publishes the new epoch before clearing the
intrusive-list head. On the documented GOMAXPROCS>1 white-box activation path,
one goroutine can publish the epoch, another can prepend a timer, and the first
can then clear the head. A later clock-rate change cannot find or remap the lost
timer.

## Required outcome

Epoch transition and list publication preserve every timer registered in the
new epoch. A controlled two-P test pauses the rollover winner, registers a
second timer, changes the host rate, and proves both timers are remapped.
