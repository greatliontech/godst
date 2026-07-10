# DST: a zero-length write on an O_SYNC handle commits the whole durable image

Lands: when the O_SYNC commit fires only for writes that wrote (ret > 0),
matching generic_write_sync

## Gap

Severity L (review-found 2026-07-10; pre-existing). `dstFile.write` runs the
`d.osync` commit after the backend write returns nil — including a
zero-length write, which the empty-effective-slice guard makes a no-op.
`commitLocked` then copies ALL pending unsynced data and metadata (including
prior writes through other handles) into the durable image. Linux's
`generic_write_sync` fires only for `ret > 0`; a real zero-length O_SYNC
write flushes nothing. The simulated durable image is "too durable": a crash
after `Write(nil)` on an O_SYNC handle durably preserves data real hardware
could lose, narrowing the crash-tear surface.

## Required outcome

The O_SYNC commit fires only when the write wrote (`n > 0`), pinned by a test
that writes unsynced data through one handle, issues `Write(nil)` on an
O_SYNC handle, crashes the host, and asserts the unsynced data was lost.
