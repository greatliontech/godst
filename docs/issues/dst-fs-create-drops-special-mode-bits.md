# File and directory creation drops representable special mode bits

Lands: when create and Chmod preserve the same setuid, setgid, and sticky bits

## Gap

Severity L. `dstMkdir`, `dstOpenFile`, and rooted creation mask requested mode
with `ModePerm`, discarding `ModeSetuid`, `ModeSetgid`, and `ModeSticky` even
though later Chmod represents them. This violates the explicit create/Chmod
consistency clause.

## Required outcome

Named and rooted file and directory creation preserve every modeled special
bit, including in the durable metadata image. Tests cover each bit before and
after crash recovery.
