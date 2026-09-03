#!/usr/bin/env bash
# Enforce a coverage floor against a `go test -coverprofile` file.
#
# Usage: check-coverage.sh <coverprofile> <floor-percent>
#
# Exits non-zero, with the actual percentage printed, if total coverage is
# below the floor. This must be invoked via its own executable bit (mode 644
# dies silently on a Linux runner) rather than `bash scripts/...`, so a lost
# +x is caught by CI itself rather than by a human reading the log.
set -euo pipefail

profile="${1:?usage: check-coverage.sh <coverprofile> <floor-percent>}"
floor="${2:?usage: check-coverage.sh <coverprofile> <floor-percent>}"

if [ ! -f "$profile" ]; then
  echo "check-coverage: no coverage profile at $profile" >&2
  exit 1
fi

total=$(go tool cover -func="$profile" | awk '/^total:/ {gsub("%","",$3); print $3}')
if [ -z "$total" ]; then
  echo "check-coverage: could not parse total coverage from $profile" >&2
  exit 1
fi

echo "total coverage: ${total}% (floor: ${floor}%)"

awk -v total="$total" -v floor="$floor" 'BEGIN { exit !(total >= floor) }' \
  || { echo "check-coverage: ${total}% is below the ${floor}% floor" >&2; exit 1; }
