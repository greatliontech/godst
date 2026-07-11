# Host declaration failures can partially publish identity and leak stamps

Lands: when Host validates and reserves all bounded state before publishing
identity, node stamps, clock state, or reboot state

## Gap

Severity M. `Host` publishes hostname/NumCPU and stamps the caller before clock
re-establishment. An invalid pre-epoch clock leaves the new identity published
while retaining the old clock. A host-table bound panic occurs before the
outer restoration defer is installed, leaving the caller stamped with a host
whose declaration failed.

## Required outcome

Host declaration is all-or-nothing on every panic path. Tests cover invalid
clock re-declaration and host-table exhaustion, then verify caller identity and
the previous host's identity, clock, power, and timers are unchanged.
