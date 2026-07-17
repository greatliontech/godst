# Plan: gmdb syscall surface — renameat2 + futex

Contract: docs/dst/design.md (amended per chunk).

- [ ] 1. renameat2(RENAME_NOREPLACE) raw dispatch: AT_FDCWD-relative
      renameat2 routed to the modeled rename with a NOREPLACE EEXIST
      arm; flags allowlist (0, NOREPLACE; EXCHANGE/WHITEOUT → EINVAL,
      the kernel's unsupported-flags shape); raw-dispatch arg path
      widened to the full six.
- [ ] 2. SYS_FUTEX model: shared (non-PRIVATE) FUTEX_WAIT with
      timeout / FUTEX_WAKE on MAP_SHARED file pages — wait-queue
      keyed (node, page offset) with a bucket lock closing the
      lost-wake window, timespec on the virtual clock, deterministic
      FIFO wake order.
