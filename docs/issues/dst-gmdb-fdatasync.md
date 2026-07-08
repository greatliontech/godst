# DST gmdb fdatasync durability boundary

Lands: 3

## Gap

gmdb uses `Fdatasync` as its crash-consistency barrier. DST currently advances the filesystem durable image through `os.File.Sync`; raw `Fdatasync` on a simulated file is fenced.

## Required outcome

`Fdatasync` is a first-class virtual-fd operation for simulated regular files. It advances the DST filesystem durability boundary that host crash recovery reads, without routing through a host descriptor. `Fdatasync` remains a distinct modeled operation from `Fsync`, even when the current in-memory filesystem commits the same gmdb-relevant durable image for both.
