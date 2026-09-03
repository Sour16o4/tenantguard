#!/usr/bin/env bash
# Run the full test suite (including the M1-M8 mutation harness) with the race
# detector and coverage, then assert that nothing silently skipped and enforce
# the coverage floor. This is the merge-blocking step for TGD-US-11 AC-6: a
# green result here is the only thing that may be trusted as "the oracle is
# proven" — an unenforced or silently-skipped gate is indistinguishable from
# no gate at all.
set -uo pipefail

COVERAGE_FLOOR="${COVERAGE_FLOOR:-60}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

go test -json -race -coverprofile=coverage.out ./... | tee test-output.json \
  | python3 "$here/assert_tests_ran.py"
assert_status=${PIPESTATUS[2]}
test_status=${PIPESTATUS[0]}

if [ "$test_status" -ne 0 ]; then
  echo "run_tests_with_assertions: go test itself failed (exit $test_status)" >&2
  exit "$test_status"
fi
if [ "$assert_status" -ne 0 ]; then
  echo "run_tests_with_assertions: skip/mutant-coverage assertion failed (exit $assert_status)" >&2
  exit "$assert_status"
fi

"$here/check-coverage.sh" coverage.out "$COVERAGE_FLOOR"
