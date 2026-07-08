# DST gmdb directory fsync durability

Lands: 3

## Gap

gmdb syncs parent directories so directory entries are durable. DST has directory handles and directory `File.Sync` state, but gmdb's raw fd path cannot currently sync a simulated directory descriptor.

## Required outcome

Opening a simulated directory yields a virtual fd that `Fsync` can target. `Fsync` on that directory commits the directory-entry durable image. A file whose content was synced but whose parent directory entry was not synced can disappear on a host crash; a synced directory entry survives according to the durable image.
