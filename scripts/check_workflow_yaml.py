#!/usr/bin/env python3
"""Parse a GitHub Actions workflow file and fail loudly if it is malformed.

GitHub silently ignores a workflow file it cannot parse — it simply never
appears as a check, with no error surfaced anywhere obvious.
A pipeline that is supposed to be merge-blocking is only merge-blocking if
GitHub can actually load it, and there is no other loud signal that it failed
to. This script is that signal, run as its own CI step.

It checks YAML syntax and a minimal structural sketch of the Actions schema —
top-level `on` and `jobs`, and every job having `runs-on` and `steps`, every
step having exactly one of `run`/`uses`. This is intentionally not a full
schema validator (that job belongs to `actionlint`, run as a separate,
authoritative CI step); this script exists so a bare syntax or gross structural
error is caught even without a third-party tool installed.

Usage: check_workflow_yaml.py <workflow.yml> [<workflow.yml> ...]
"""
import sys

try:
    import yaml
except ImportError:
    print("check_workflow_yaml: PyYAML is not installed", file=sys.stderr)
    sys.exit(1)


def check(path: str) -> list[str]:
    errors = []
    try:
        with open(path, encoding="utf-8") as f:
            doc = yaml.safe_load(f)
    except yaml.YAMLError as e:
        return [f"{path}: not valid YAML: {e}"]
    except OSError as e:
        return [f"{path}: cannot read file: {e}"]

    if not isinstance(doc, dict):
        return [f"{path}: top level is not a mapping"]

    # YAML parses the bare key `on` as the boolean True unless quoted, which
    # is itself a classic Actions-workflow footgun worth checking for.
    if "on" not in doc and True not in doc:
        errors.append(f"{path}: missing top-level 'on' key")
    if "jobs" not in doc:
        errors.append(f"{path}: missing top-level 'jobs' key")
        return errors

    jobs = doc["jobs"]
    if not isinstance(jobs, dict) or not jobs:
        errors.append(f"{path}: 'jobs' is not a non-empty mapping")
        return errors

    for job_name, job in jobs.items():
        if not isinstance(job, dict):
            errors.append(f"{path}: job '{job_name}' is not a mapping")
            continue
        if "runs-on" not in job:
            errors.append(f"{path}: job '{job_name}' has no 'runs-on'")
        steps = job.get("steps")
        if not isinstance(steps, list) or not steps:
            errors.append(f"{path}: job '{job_name}' has no non-empty 'steps' list")
            continue
        for i, step in enumerate(steps):
            if not isinstance(step, dict):
                errors.append(f"{path}: job '{job_name}' step {i} is not a mapping")
                continue
            has_run = "run" in step
            has_uses = "uses" in step
            if has_run == has_uses:
                errors.append(
                    f"{path}: job '{job_name}' step {i} must have exactly one of "
                    f"'run'/'uses', has run={has_run} uses={has_uses}")
    return errors


def main() -> int:
    paths = sys.argv[1:]
    if not paths:
        print("usage: check_workflow_yaml.py <workflow.yml> [...]", file=sys.stderr)
        return 2
    all_errors = []
    for p in paths:
        all_errors.extend(check(p))
    if all_errors:
        for e in all_errors:
            print(f"check_workflow_yaml: {e}", file=sys.stderr)
        return 1
    print(f"check_workflow_yaml: {len(paths)} workflow(s) parsed and structurally sane")
    return 0


if __name__ == "__main__":
    sys.exit(main())
