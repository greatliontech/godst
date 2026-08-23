#!/bin/sh
# Stale-base check (docs/dst/releases.md, Patch cadence): exit 1 when
# upstream has a newer FINAL point release than $1 on the same minor —
# or when the question cannot be answered. The check fails CLOSED: an
# upstream query failure or an empty match refuses rather than passes,
# because a pass here is what lets a release carry no upstream security
# patches. rc/beta tags never match the final-release pattern. Shared by
# the release gate (the refusal) and the tagwatch workflow (the
# observability half); runnable locally: .github/scripts/stale-base.sh go1.27.0
# Output contract: exactly one line per outcome (tagwatch annotates it).
set -eu
base=${1:-}
[ -n "$base" ] || { echo "usage: stale-base.sh goX.Y.Z"; exit 2; }
minor=${base%.*}
raw=$(git ls-remote --tags https://github.com/golang/go "refs/tags/${minor}.*") || {
  echo "upstream tag query failed — refusing to judge staleness"; exit 1; }
latest=$(printf '%s\n' "$raw" | awk -F/ '{print $NF}' | grep -E '^go[0-9]+\.[0-9]+\.[0-9]+$' | sort -V | tail -1 || true)
[ -n "$latest" ] || {
  echo "no final point release found upstream for $minor — refusing to judge staleness"; exit 1; }
if [ "$(printf '%s\n%s\n' "$base" "$latest" | sort -V | tail -1)" != "$base" ]; then
  echo "STALE: base $base trails upstream $latest — the port lands before, or as, the line's next release (releases.md, Patch cadence)"
  exit 1
fi
echo "base $base is current (upstream latest on $minor: $latest)"
