# DST audit: crash restore never advances the durable image; a second crash redraws the disk

Lands: when a host-crash restore commits the restored image as the new durable image

## Gap

Severity H (full-surface audit, 2026-07-10; reproduced). `dstRestoreNodeLocked`
(`src/os/dst_host_crash.go:90-106`) writes the (possibly torn) restored image
into `node.data`/`node.entries` but leaves `node.synced`/`node.syncedEntries`
at the pre-crash durable image. After reboot, disk and page cache agree and
nothing is dirty, so by the model's own definition a second `CrashHost` must be
a no-op; instead every page where torn-live differs from stale-durable is
redrawn, and landed-but-unsynced directory entries re-flip. Reproduced: write
8 KiB durable, overwrite unsynced, `CrashHost`, snapshot, `CrashHost` again
with zero intervening writes → 6745 bytes changed between the two post-crash
images (seed 2). This persists an earlier durable state no real crash ordering
can produce — bytes on the platter revert with nothing having written — the
DST-FAULT-SOUND false-positive class the tear machinery's own comment forbids.

## Required outcome

After a host-crash restore, the restored image is committed as the new durable
image (it is, by definition, what the platter holds). A second crash with no
intervening writes changes nothing. Pinned by a double-crash test asserting
byte-identical post-crash images, torn and untorn.
