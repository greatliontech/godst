# Pending feature: deterministic file/pipe/stdio I/O under DST

**Lands:** pending feature (the third I/O feature on the Roadmap, after network and
disk). No `Lands:` chunk — planned work.

## Goal

Cover the I/O that the network and filesystem features do not: pipes
(`os.Pipe`, `io.Pipe` is already in-memory), the standard streams
(`os.Stdin`/`Stdout`/`Stderr`), and any other OS-backed I/O a program under test
reaches for. Under `simulation.Run` these become **in-memory and deterministic**,
so a program that reads stdin or writes a pipe is reproducible and does not block
on or perturb the real process's I/O.

## Why a separate feature

It is the catch-all once the two big axes (network, disk) are virtualized. Much of
it is small and may partly fall out of the disk feature (`os.Pipe` is fd-backed
like files). It is listed separately so the remaining surface is tracked rather
than assumed covered.

## Scope (rough)

- `os.Pipe` → in-memory pipe (synctest-durable, like the network conns).
- Standard streams → in-memory buffers under a run (a program that writes Stdout
  gets a deterministic, capturable stream; reading Stdin yields seeded/configured
  input rather than blocking on the real terminal).
- Audit the remaining `os`/`io` surface a typical program touches and bring the
  nondeterministic bits in-memory.

## Open questions to settle when it lands

- How much of stdio to virtualize vs. leave as the program's responsibility
  (a test usually controls its own stdin/stdout already).
- ~~Whether `os.Pipe` belongs here or with the disk feature~~ — SETTLED at the
  disk feature's design step: `os.Pipe` lands HERE, as a stream-shaped backend
  behind the `os.File` dst seam the disk feature built (it is fenced under a
  run until then; see design.md "In-memory deterministic filesystem", the
  backend-not-fd paragraph).

## Contract note

Same as network/disk: this brings more real I/O into the fork; document the model
in design.md when it lands.
