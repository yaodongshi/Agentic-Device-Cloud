"""Eval metrics: report building and the pass-rate/failure-list summary (I23)."""

from __future__ import annotations

from evals.models import EvalCaseResult, EvalReport


def build_report(
    run_id: str,
    suite: str,
    started_at: str,
    results: list[EvalCaseResult],
) -> EvalReport:
    """Build the structured report from per-case results."""
    passed = sum(1 for result in results if result.passed)
    return EvalReport(
        run_id=run_id,
        suite=suite,
        started_at=started_at,
        total=len(results),
        passed=passed,
        failed=len(results) - passed,
        results=results,
    )


def summarize(report: EvalReport) -> str:
    """Human-readable summary: pass rate plus the failure list."""
    lines = [
        f"run {report.run_id} suite '{report.suite}'",
        (
            f"total={report.total} passed={report.passed} failed={report.failed} "
            f"pass_rate={report.pass_rate:.1%}"
        ),
    ]
    for case in report.failures:
        lines.append(
            f"  FAIL {case.name} ({case.tool}): expected {case.expect_status}, "
            f"got {case.actual_status} - {case.detail or ''}"
        )
    return "\n".join(lines)
