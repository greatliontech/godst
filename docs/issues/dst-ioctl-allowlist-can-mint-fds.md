# The unrestricted ioctl allowlist can mint host descriptors

Lands: when ioctl admission is request-aware and excludes every resource-
minting or host-mutating request

## Gap

Severity H. `dstSyscallAllowedTrap` admits all `SYS_IOCTL` calls to preserve
isatty probes. Linux ioctls include descriptor-minting requests such as
`TIOCGPTPEER`; given an inherited `/dev/ptmx` fd, a bubble can obtain a real
slave-PTY fd and use it through the host-fd allowlist.

## Required outcome

Only explicitly safe probe requests can reach the host. A test creates a PTY
outside the run, issues `TIOCGPTPEER` inside it, requires deterministic
refusal, and verifies the host fd census is unchanged.
