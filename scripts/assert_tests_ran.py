#!/usr/bin/env python3
"""Assert that the oracle and CLI test suites actually ran against a real
PostgreSQL instance, rather than silently skipping.

Both internal/oracle and cmd/tenantguard gate their integration tests on
TGD_TEST_DSN and skip loudly (t.Skip, with a message) when it is unset. A CI
job with no Postgres service, or one that is unreachable, would see every one
of those tests report PASS-by-skip and the job would go green having proven
nothing — the exact failure mode this script exists to catch.

It reads `go test -json` output (piped in on stdin) and fails if:
  - any test anywhere in the given packages was skipped, or
  - the eight named M1-M8 mutation-harness subtests did not all report pass.

Usage:
    go test -json ./internal/oracle/... ./cmd/tenantguard/... | \
        ./scripts/assert_tests_ran.py
"""
import json
import sys

EXPECTED_MUTANTS = [
    "TestMutationHarnessM1ThroughM8/M1:",
    "TestMutationHarnessM1ThroughM8/M2:",
    "TestMutationHarnessM1ThroughM8/M3:",
    "TestMutationHarnessM1ThroughM8/M4:",
    "TestMutationHarnessM1ThroughM8/M5:",
    "TestMutationHarnessM1ThroughM8/M6:",
    "TestMutationHarnessM1ThroughM8/M7:",
    "TestMutationHarnessM1ThroughM8/M8:",
]


def main() -> int:
    skipped = []
    passed_tests = set()
    failed = []
    saw_any_event = False

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            ev = json.loads(line)
        except json.JSONDecodeError:
            continue
        saw_any_event = True
        action = ev.get("Action")
        test = ev.get("Test")
        if not test:
            continue
        if action == "skip":
            skipped.append(f"{ev.get('Package', '?')}.{test}")
        elif action == "pass":
            passed_tests.add(test)
        elif action == "fail":
            failed.append(f"{ev.get('Package', '?')}.{test}")

    if not saw_any_event:
        print("assert_tests_ran: no test events read from stdin at all — "
              "the test run itself produced nothing", file=sys.stderr)
        return 1

    if failed:
        print(f"assert_tests_ran: {len(failed)} test(s) failed: {failed}",
              file=sys.stderr)
        return 1

    if skipped:
        print(f"assert_tests_ran: {len(skipped)} test(s) were SKIPPED — "
              "Postgres was likely unreachable (TGD_TEST_DSN unset or bad) "
              "and this job verified nothing:", file=sys.stderr)
        for s in skipped:
            print(f"  {s}", file=sys.stderr)
        return 1

    missing = [want for want in EXPECTED_MUTANTS
               if not any(want in got for got in passed_tests)]
    if missing:
        print("assert_tests_ran: the M1-M8 mutation harness did not report all "
              "eight expected subtests as passed. Missing:", file=sys.stderr)
        for m in missing:
            print(f"  {m}", file=sys.stderr)
        print(f"(saw {len(passed_tests)} passed tests total)", file=sys.stderr)
        return 1

    print(f"assert_tests_ran: {len(passed_tests)} tests passed, 0 skipped, "
          f"all 8 mutation-harness subtests confirmed present")
    return 0


if __name__ == "__main__":
    sys.exit(main())
