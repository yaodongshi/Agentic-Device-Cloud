"""Application-level Pydantic models for the eval harness (LLD 3.6.3).

Wire protocol types (JSON-RPC envelopes, MCP tools) come from the
adc_core_sdk package; this module only defines eval-domain models.
"""

from __future__ import annotations

from typing import Any

from pydantic import BaseModel, Field


class EvalCaseSpec(BaseModel):
    """A single eval case: call a tool and compare the outcome status."""

    name: str = Field(min_length=1, max_length=64, description="Case name.")
    tool: str = Field(description="Target tool in deviceCode::toolName form.")
    args: dict[str, Any] = Field(default_factory=dict, description="Tool arguments.")
    expect_status: str = Field(
        default="ok",
        pattern="^(ok|blocked|error)$",
        description="Expected outcome status: ok, blocked or error.",
    )


class EvalRunSpec(BaseModel):
    """A full eval run over an ordered list of cases."""

    suite: str = Field(min_length=1, max_length=128)
    cases: list[EvalCaseSpec] = Field(min_length=1, max_length=500)
    tenant: str | None = Field(default=None, description="Empty means the local mock tenant.")


class EvalCaseResult(BaseModel):
    """Outcome of a single case, including elapsed time and failure detail."""

    name: str
    tool: str
    expect_status: str
    actual_status: str
    passed: bool
    elapsed_ms: int = Field(ge=0)
    detail: str | None = None


class EvalReport(BaseModel):
    """Structured eval report (I23): pass rate plus per-case results."""

    run_id: str
    suite: str
    started_at: str
    total: int
    passed: int
    failed: int
    results: list[EvalCaseResult]

    @property
    def pass_rate(self) -> float:
        """Pass rate in [0.0, 1.0]; 0.0 when the report has no cases."""
        return self.passed / self.total if self.total else 0.0

    @property
    def failures(self) -> list[EvalCaseResult]:
        """Failure list: every case whose outcome did not match expectations."""
        return [result for result in self.results if not result.passed]
