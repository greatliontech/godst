#!/bin/sh
# Fails if the checkout carries a linked executable — ELF ET_EXEC/ET_DYN, a
# Mach-O executable (MH_EXECUTE, MH_PRELOAD, MH_DYLINKER, MH_BUNDLE), or a
# PE image — in a tracked path outside a testdata directory
# (docs/dst/releases.md "Continuous integration"). Upstream's tracked .syso
# files are relocatable objects (ELF ET_REL / Mach-O MH_OBJECT / COFF) and its
# executable fixtures live under testdata, so the predicate flags exactly a
# stray build product such as a `compile` binary at the repo root. Every
# tracked file is inspected (a small PIE is as much an artifact as a large
# one); the violation list is collected and printed before exiting nonzero.
set -eu
viol=$(git ls-files | grep -v '\(^\|/\)testdata/' | while IFS= read -r f; do
  [ -f "$f" ] && [ ! -L "$f" ] || continue
  m=$(head -c 4 "$f" | od -An -tx1 | tr -d ' \n')
  case "$m" in
    7f454c46)
      ei=$(head -c 6 "$f" | tail -c 1 | od -An -tu1 | tr -d ' ')   # EI_DATA: 1 = little-endian
      t=$(head -c 18 "$f" | tail -c 2 | od -An -tx1 | tr -d ' \n')   # e_type, 2 bytes
      [ "$ei" = 1 ] && t="${t#??}${t%??}"
      case "$t" in 0002|0003) echo "linked ELF executable tracked: $f";; esac;;
    feedface|feedfacf)   # Mach-O, big-endian magic: filetype at offset 12, BE
      ft=$(head -c 16 "$f" | tail -c 4 | od -An -tx1 | tr -d ' \n')
      case "$ft" in 00000002|00000005|00000007|00000008) echo "Mach-O executable tracked: $f";; esac;;
    cefaedfe|cffaedfe)   # Mach-O, little-endian magic: filetype LE
      ft=$(head -c 16 "$f" | tail -c 4 | od -An -tx1 | tr -d ' \n')
      ft=$(printf '%s' "$ft" | sed 's/\(..\)\(..\)\(..\)\(..\)/\4\3\2\1/')
      case "$ft" in 00000002|00000005|00000007|00000008) echo "Mach-O executable tracked: $f";; esac;;
    4d5a*)   # MZ stub: a PE image has the PE\0\0 signature at the offset stored (LE) at 0x3c
      off=$(head -c 64 "$f" | tail -c 4 | od -An -tx1 | tr -d ' \n')
      case "$off" in ????????)
        off=$(printf '%s' "$off" | sed 's/\(..\)\(..\)\(..\)\(..\)/\4\3\2\1/'); off=$((0x$off))
        if [ "$off" -gt 0 ] && [ "$off" -lt 4096 ] && [ "$(head -c $((off+4)) "$f" | tail -c 4 | od -An -tx1 | tr -d ' \n')" = 50450000 ]; then
          echo "PE image tracked: $f"
        fi;;
      esac;;
  esac
done)
if [ -n "$viol" ]; then printf '%s\n' "$viol"; exit 1; fi
echo "no tracked build artifacts"
