# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number when an active plan exists, a self-contained condition, or
"pending feature" for planned roadmap work). At close-out, lasting rationale is
promoted into a kept-current artifact and the resolved entry is deleted.

## Open

- **process-identity divergence modeling** — Lands: when a client needs
  pid REUSE (same pid, new start-time) or pid-namespace divergence
  constructible in-simulation. Pids are allocated monotonically and
  never reused; every process sees namespace pid:[1] — so a client's
  reused-pid and cross-namespace staleness-classification legs cannot
  be exercised end-to-end.
- **bubble-scoped timezone cache** — Lands: when a client needs
  non-UTC zone data in-simulation, or the determinism sweep gains a
  TZ-perturbation leg. time's zone cache is process-wide: a zone
  loaded by HOST code before a run stays visible inside later bubbles
  without a file read, bypassing the ENOENT-under-fence answer in
  time.open (src/time/dst_tz.go) — a residual host-dependence the
  current fix narrows but does not close.
- **`link(2)` model** — Lands: when a client needs the hard-link publish
  idiom (link-then-unlink atomic no-clobber, the NFS retransmission
  quirk) exercised in-simulation. `os.Link` currently answers the
  unsupported-FS refusal; clients degrade to their rename fallback,
  which the modeled renameat2 serves.
- **per-boot host identity (`/proc/sys/kernel/random/boot_id` + boottime
  reset)** — Lands: when a client needs cross-reboot epoch invalidation
  testable in-sim. The procfs overlay models no `/proc/sys` surface; host
  identity is immutable once published, so nothing regenerates per boot,
  and BOOTTIME deliberately does not reset across a `Host` re-declaration
  (design.md records boottime == monotonic until a suspend model exists).
  Faithful semantics need per-boot host state seeded from (seed, host,
  boot count), a `/proc/sys` leaf, and a boottime-reset decision. Clients
  reading boot_id best-effort degrade to their zero-epoch path today.
