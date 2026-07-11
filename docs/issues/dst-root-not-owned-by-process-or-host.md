# Simulated Root handles are not owned by process or host teardown

Lands: when Root handles participate in process and host capability teardown

## Gap

Severity M. `dstNewRoot` records node, disk, and epoch only. It does not record
host/process ownership or enter the open-file registry, so process exit and
host crash cannot close it. A retained Root remains usable after its owning
process exits or after reboot, although the modeled directory descriptor should
be gone.

## Required outcome

Root is an owned open capability with the same teardown and closed identity as
other simulated handles. Tests retain a Root across process exit, process
crash, and host crash/reboot and require every rooted operation to fail closed.
