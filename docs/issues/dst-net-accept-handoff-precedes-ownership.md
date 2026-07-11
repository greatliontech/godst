# Accept handoff precedes connection ownership registration

Lands: when every accepted or queued endpoint is lifecycle-owned before process
or host teardown can observe it

## Gap

Severity H. `dstDial` sends the server endpoint to the listener before either
end is entered in `dstConns`. The accepting process can return, close the end,
or lose its host before the dialer resumes. Teardown scans no connection, and
the dialer can then register stale endpoints and return success, leaving a
phantom binding or a client that hangs instead of seeing EOF/reset.

## Required outcome

Establishment publishes endpoint ownership atomically with the accept/backlog
state, and every failure removes it exactly once. Schedule-sweep tests cover
accept-then-return, accepted-end close, and host crash before dialer resume.
