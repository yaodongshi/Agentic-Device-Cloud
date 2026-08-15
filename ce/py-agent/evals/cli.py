"""CLI entry point: python -m evals (or the `eval` console script).

Runs an eval suite offline and prints the structured summary (pass rate plus
failure list); optionally writes the full report as JSON.
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from evals.harness import EvalHarness
from evals.metrics import summarize
from evals.models import EvalRunSpec


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="python -m evals",
        description="Run ADC eval suites and write a structured report.",
    )
    parser.add_argument("--suite", help="suite name to run (looked up in --suites-dir)")
    parser.add_argument(
        "--suites-dir", default="suites", help="directory holding <suite>.json files"
    )
    parser.add_argument(
        "--tool-filter", help="only run cases whose tool contains this substring"
    )
    parser.add_argument("--output", help="write the report as JSON to this file")
    parser.add_argument(
        "--list-suites", action="store_true", help="list available suites and exit"
    )
    return parser


def main(argv: list[str] | None = None) -> int:
    """Run the eval CLI; returns the process exit code."""
    parser = _build_parser()
    args = parser.parse_args(argv)
    harness = EvalHarness(Path(args.suites_dir))

    if args.list_suites:
        print("\n".join(harness.list_suites()))
        return 0
    if not args.suite:
        parser.error("--suite is required unless --list-suites is given")

    try:
        spec = harness.load_suite(args.suite)
    except FileNotFoundError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

    if args.tool_filter:
        filtered_cases = [case for case in spec.cases if args.tool_filter in case.tool]
        if not filtered_cases:
            print("error: tool filter removed all cases", file=sys.stderr)
            return 2
        spec = EvalRunSpec(suite=spec.suite, tenant=spec.tenant, cases=filtered_cases)

    report = harness.run(spec)
    print(summarize(report))
    if args.output:
        output_path = Path(args.output)
        try:
            output_path.write_text(
                json.dumps(report.model_dump(), indent=2, ensure_ascii=False) + "\n",
                encoding="utf-8",
            )
        except OSError as exc:
            print(f"error: cannot write report: {exc}", file=sys.stderr)
            return 2
        print(f"report written to {output_path}")
    return 0
