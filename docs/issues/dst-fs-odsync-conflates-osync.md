# DST fs: O_DSYNC conflates to full O_SYNC (metadata over-commit)

Lands: when an O_DSYNC handle's writes commit through the data-only commit
(commitDataLocked), or the conflation is recorded beside the O_SYNC rule

## Gap

Severity L (audit-found 2026-07-11). The open-flag check `flag&O_SYNC != 0`
also matches syscall.O_DSYNC (0x1000 is a bit of Linux's 0x101000 O_SYNC), so
an O_DSYNC open commits metadata (mtime, via commitLocked) that real O_DSYNC
omits. The data-only commitDataLocked exists (the datasync path) but is not
wired to O_DSYNC handles. Metadata-only over-durability — the conservative
direction, but a modeled distinction the code already owns machinery for.

## Required outcome

O_DSYNC handles commit data only, or design.md's durability section records
the conflation as deliberate.
