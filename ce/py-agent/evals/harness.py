"""EvalHarness (I23): eval execution and reporting, shared by CLI and API."""

from __future__ import annotations

import time
from datetime import UTC, datetime
from pathlib import Path
from uuid import uuid4

from evals import loader
from evals.executor import MockExecutor, ToolExecutor
from evals.metrics import build_report
from evals.models import EvalCaseResult, EvalCaseSpec, EvalReport, EvalRunSpec


class EvalHarness:
    """Run eval suites and keep recent reports in memory (V1.0: no DB)."""

    def __init__(self, suites_dir: Path, executor: ToolExecutor | None = None) -> None:
        self.suites_dir = suites_dir
        self._executor: ToolExecutor = executor or MockExecutor()
        self._reports: dict[str, EvalReport] = {}

    def run(self, spec: EvalRunSpec) -> EvalReport:
        """Execute every case in the spec and store the resulting report."""
        run_id = uuid4().hex
        started_at = datetime.now(UTC).isoformat()
        results = [self._run_case(case) for case in spec.cases]
        report = build_report(run_id, spec.suite, started_at, results)
        self._reports[run_id] = report
        return report

    def load_suite(self, name: str) -> EvalRunSpec:
        """Load a suite by name from the configured suites directory."""
        return loader.load_suite(self.suites_dir, name)

    def list_suites(self) -> list[str]:
        """List suite names available in the configured suites directory."""
        return loader.list_suites(self.suites_dir)

    def report(self, run_id: str) -> EvalReport | None:
        """Return the stored report for run_id, or None if unknown."""
        return self._reports.get(run_id)

    def _run_case(self, case: EvalCaseSpec) -> EvalCaseResult:
        start = time.perf_counter()
        outcome = self._executor.execute(case)
        elapsed_ms = round((time.perf_counter() - start) * 1000)
        return EvalCaseResult(
            name=case.name,
            tool=case.tool,
            expect_status=case.expect_status,
            actual_status=outcome.status,
            passed=outcome.status == case.expect_status,
            elapsed_ms=elapsed_ms,
            detail=outcome.detail,
        )
