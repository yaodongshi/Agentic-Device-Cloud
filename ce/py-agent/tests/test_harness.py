"""Eval harness execution and report tests."""

from __future__ import annotations

from evals.executor import EvalOutcome
from evals.harness import EvalHarness
from evals.models import EvalCaseSpec, EvalRunSpec


class FakeExecutor:
    """Deterministic executor used to pin harness behaviour."""

    def __init__(self, status: str = "ok") -> None:
        self.status = status

    def execute(self, case: EvalCaseSpec) -> EvalOutcome:
        return EvalOutcome(self.status, f"fake: {case.tool}")


def test_run_all_pass(harness: EvalHarness) -> None:
    spec = harness.load_suite("smoke")
    report = harness.run(spec)
    assert report.total == 4
    assert report.passed == 4
    assert report.failed == 0
    assert report.pass_rate == 1.0
    assert report.failures == []
    assert report.suite == "smoke"
    assert len(report.results) == 4
    assert all(result.elapsed_ms >= 0 for result in report.results)


def test_report_roundtrip(harness: EvalHarness) -> None:
    report = harness.run(harness.load_suite("smoke"))
    assert harness.report(report.run_id) == report


def test_report_unknown_run_id(harness: EvalHarness) -> None:
    assert harness.report("missing-run-id") is None


def test_failure_is_listed_with_actual_status(harness: EvalHarness) -> None:
    spec = EvalRunSpec(
        suite="blocked-bad-expectation",
        cases=[EvalCaseSpec(name="blocked tool", tool="dev-002::set_mode", expect_status="ok")],
    )
    report = harness.run(spec)
    assert report.failed == 1
    assert report.pass_rate == 0.0
    assert len(report.failures) == 1
    failure = report.failures[0]
    assert failure.name == "blocked tool"
    assert failure.actual_status == "blocked"
    assert "approval" in (failure.detail or "")


def test_injected_executor_decides_status(harness: EvalHarness) -> None:
    injected = EvalHarness(harness.suites_dir, executor=FakeExecutor(status="error"))
    spec = EvalRunSpec(
        suite="injected",
        cases=[EvalCaseSpec(name="any tool", tool="dev-001::reboot")],
    )
    report = injected.run(spec)
    assert report.passed == 0
    assert report.results[0].actual_status == "error"
