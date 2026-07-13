# Verbose-transcript same-seed divergence under concurrent host load

`TestVerboseOnOffSameSeedTranscript`
(`src/testing/simulation/verbose_linux_test.go`) failed once — "same
seed, -v on vs off: transcripts differ" — when the `testing/simulation`
dst leg ran concurrently with the `os` and `net` dst legs on a loaded
host, and passes repeatedly when the package runs alone. The test
compares two same-seed run transcripts inside one binary, so a
load-dependent divergence means a wall-clock influence still reaches
the decision stream somewhere around the `-v` machinery or its
surroundings, despite the framework stream's scheduler-invisible
RawSyscall form.

This is a same-seed determinism escape of exactly the class the
capability-write entersyscall window issue records (that window is
demonstrated for inherited-file capability writes; this failure may be
that mechanism reached another way, or a distinct leak — undiagnosed).
Reproduce by running the three dst packages concurrently under host
load:

```
GOROOT=$PWD TMPDIR=$PWD/.tmp ./bin/go test -tags dst -count=1 \
  os net testing/simulation   # six pre-existing skips apply
```

Lands: with the determinism-escape sweep, alongside the
capability-write entersyscall window item.
