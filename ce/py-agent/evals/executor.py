"""Tool executors for the eval harness.

V1.0 runs fully offline against a deterministic mock device catalog built
from the adc_core_sdk wire types; a real gateway executor is a V1.5 seam
(orchestration runner, LLD 3.6.4).
"""

from __future__ import annotations

from typing import NamedTuple, Protocol

from adc_core_sdk import MCPTool, ToolCallResult, ToolContent, ToolsListResult

from evals.models import EvalCaseSpec

STATUS_OK = "ok"
STATUS_BLOCKED = "blocked"
STATUS_ERROR = "error"


class EvalOutcome(NamedTuple):
    """Executor verdict for one case: status plus optional detail."""

    status: str
    detail: str | None = None


class ToolExecutor(Protocol):
    """Anything that can execute one eval case against a tool target."""

    def execute(self, case: EvalCaseSpec) -> EvalOutcome: ...


def default_catalog() -> ToolsListResult:
    """Deterministic mock device catalog used when the harness runs offline."""
    return ToolsListResult(
        tools=[
            MCPTool(
                name="dev-001::reboot",
                description="Reboot the device",
                inputSchema={
                    "type": "object",
                    "properties": {"delay_ms": {"type": "integer"}},
                },
                riskLevel=2,
                schemaVersion="v1",
            ),
            MCPTool(
                name="dev-001::get_telemetry",
                inputSchema={"type": "object"},
                riskLevel=1,
                schemaVersion="v1",
            ),
            MCPTool(
                name="dev-002::set_mode",
                description="Switch operating mode",
                inputSchema={
                    "type": "object",
                    "properties": {"mode": {"type": "string"}},
                },
                riskLevel=3,
                schemaVersion="v1",
            ),
            MCPTool(
                name="dev-002::get_status",
                inputSchema={"type": "object"},
                schemaVersion="v1",
            ),
        ]
    )


class MockExecutor:
    """Simulate tools/call against the catalog, including HITL-style blocks.

    Outcome mapping:
    - unknown tool -> error
    - tool risk level 3 -> blocked (simulated "approve before use")
    - otherwise -> ok, with a deterministic ToolCallResult payload as detail
    """

    def __init__(self, catalog: ToolsListResult | None = None) -> None:
        self._by_name = {tool.name: tool for tool in (catalog or default_catalog()).tools}

    def execute(self, case: EvalCaseSpec) -> EvalOutcome:
        tool = self._by_name.get(case.tool)
        if tool is None:
            return EvalOutcome(STATUS_ERROR, f"tool not found: {case.tool}")
        if tool.effective_risk == 3:
            return EvalOutcome(
                STATUS_BLOCKED, f"tool {case.tool} requires approval (risk level 3)"
            )
        result = ToolCallResult(
            content=[ToolContent(type="text", text=f"mock ok: {case.tool}")],
            isError=False,
        )
        return EvalOutcome(STATUS_OK, result.to_wire_json())
