# Issue docs

Tracked follow-ups and **pending features**. Each entry carries a `Lands:` trigger
(a chunk number when an active plan exists, a self-contained condition, or
"pending feature" for planned roadmap work). At close-out, lasting rationale is
promoted into a kept-current artifact and the resolved entry is deleted.

## Open

- **per-boot host identity (`/proc/sys/kernel/random/boot_id` + boottime
  reset)** — Lands: when a client needs cross-reboot epoch invalidation
  testable in-sim. The procfs overlay models no `/proc/sys` surface; host
  identity is immutable once published, so nothing regenerates per boot,
  and BOOTTIME deliberately does not reset across a `Host` re-declaration
  (design.md records boottime == monotonic until a suspend model exists).
  Faithful semantics need per-boot host state seeded from (seed, host,
  boot count), a `/proc/sys` leaf, and a boottime-reset decision. Clients
  reading boot_id best-effort degrade to their zero-epoch path today.
