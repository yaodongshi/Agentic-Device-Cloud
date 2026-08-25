"""A2A agent surface + deterministic four-agent pipeline (FR-022/024, B7).

V1 ships a rule-driven pipeline (Planner -> DeviceSelector -> Executor ->
HITLApprover) with the A2A task state machine and Agent Card publication.
LangGraph-style durable execution is the documented V2 evolution seam;
the pipeline is kept deterministic so the eval suite can grade it exactly.
"""

from __future__ import annotations

import uuid
from collections.abc import Awaitable, Callable
from enum import Enum
from threading import RLock
from typing import Annotated, Any, Literal

import httpx
from fastapi import APIRouter, Depends, HTTPException, Request
from pydantic import BaseModel, Field

router = APIRouter(prefix="/v2/agents/a2a", tags=["a2a"])

AGENT_NAME = "ADC Device Orchestrator"
AGENT_DESCRIPTION_EN = "Orchestrates physical devices (CNC/PLC/AGV) behind HITL approval."
AGENT_DESCRIPTION_ZH = "在 HITL 审批约束下编排物理设备（CNC/PLC/AGV）。"
TASK_TYPES = ["diagnose", "schedule", "inspect", "maintain"]
SCOPE_TASKS_READ = "a2a.tasks:read"
SCOPE_TASKS_WRITE = "a2a.tasks:write"
DEFAULT_ADMIN_INTERNAL_URL = "http://adc:8080"


class TaskState(Enum):
    SUBMITTED = "submitted"
    WORKING = "working"
    INPUT_REQUIRED = "input-required"
    COMPLETED = "completed"
    FAILED = "failed"
    REJECTED = "rejected"


class AgentCard(BaseModel):
    name: str
    description: str
    url: str
    version: str = "1.0.0"
    capabilities: dict[str, Any]
    default_input_modes: list[str] = ["text"]
    default_output_modes: list[str] = ["text"]
    skills: list[dict[str, Any]]


class A2ATaskRequest(BaseModel):
    task_type: str
    goal: str
    devices: list[str] = Field(default_factory=list)
    tenant_id: str | None = None


class A2ATask(BaseModel):
    task_id: str
    tenant_id: str
    application_id: str
    state: TaskState
    task_type: str
    goal: str
    devices: list[str]
    steps: list[dict[str, Any]] = Field(default_factory=list)
    message: str = ""


class ApplicationPrincipal(BaseModel):
    active: Literal[True]
    application_id: str = Field(min_length=1)
    tenant_id: str = Field(min_length=1)
    required_scope: str


Introspector = Callable[[Request, str], Awaitable[ApplicationPrincipal]]


async def introspect_application(request: Request, required_scope: str) -> ApplicationPrincipal:
    headers = {
        name: value
        for name in ("Authorization", "X-ADC-Application-Credential")
        if (value := request.headers.get(name))
    }
    url = request.app.state.admin_internal_url.rstrip("/") + "/v1/developer/introspection"
    try:
        async with httpx.AsyncClient(
            timeout=3.0, transport=request.app.state.auth_transport
        ) as client:
            response = await client.post(
                url, json={"required_scope": required_scope}, headers=headers
            )
    except httpx.HTTPError as exc:
        raise HTTPException(status_code=503, detail="credential verification unavailable") from exc
    if response.status_code in (401, 403):
        raise HTTPException(
            status_code=response.status_code, detail="application credential rejected"
        )
    if response.status_code != 200:
        raise HTTPException(status_code=503, detail="credential verification unavailable")
    try:
        principal = ApplicationPrincipal.model_validate(response.json())
    except (ValueError, TypeError) as exc:
        raise HTTPException(
            status_code=503, detail="invalid credential verification response"
        ) from exc
    if principal.required_scope != required_scope:
        raise HTTPException(status_code=503, detail="invalid credential verification response")
    return principal


async def require_read(request: Request) -> ApplicationPrincipal:
    introspector: Introspector = request.app.state.introspector
    return await introspector(request, SCOPE_TASKS_READ)


async def require_write(request: Request) -> ApplicationPrincipal:
    introspector: Introspector = request.app.state.introspector
    return await introspector(request, SCOPE_TASKS_WRITE)


def build_agent_card(base_url: str = "http://localhost:18080") -> AgentCard:
    skills = [
        {
            "id": t,
            "name": t,
            "description": f"{t} physical devices under HITL approval",
            "tags": ["device", "mcp", "hitl"],
        }
        for t in TASK_TYPES
    ]
    return AgentCard(
        name=AGENT_NAME,
        description=AGENT_DESCRIPTION_EN,
        url=f"{base_url}/v2/agents/a2a",
        capabilities={"streaming": False, "push_notifications": False},
        skills=skills,
    )


class Orchestrator:
    """Deterministic four-agent pipeline (doc/07 3.3)."""

    HIGH_RISK_KEYWORDS = ("set", "write", "move", "reboot", "stop", "override")

    def run(self, req: A2ATaskRequest, principal: ApplicationPrincipal) -> A2ATask:
        task_id = str(uuid.uuid4())
        steps: list[dict[str, Any]] = []
        # Planner: validate task type and derive an action per device.
        if req.task_type not in TASK_TYPES:
            raise HTTPException(status_code=400, detail=f"unsupported task_type {req.task_type}")
        # DeviceSelector: dedupe and order devices.
        devices = list(dict.fromkeys(d for d in req.devices if d))
        # Executor: produce a tool-call plan per device; HITLApprover marks
        # high-risk steps input-required.
        any_high_risk = False
        for dev in devices:
            tool = f"{req.task_type}_action"
            risk = (
                2
                if any(k in tool for k in self.HIGH_RISK_KEYWORDS) or req.task_type == "maintain"
                else 1
            )
            if risk >= 2:
                any_high_risk = True
            steps.append(
                {
                    "agent": "executor",
                    "device": dev,
                    "tool": tool,
                    "risk_level": risk,
                    "approval": "required" if risk >= 2 else "none",
                }
            )
        if not devices:
            raise HTTPException(status_code=400, detail="devices must not be empty")
        task = A2ATask(
            task_id=task_id,
            tenant_id=principal.tenant_id,
            application_id=principal.application_id,
            state=TaskState.INPUT_REQUIRED if any_high_risk else TaskState.WORKING,
            task_type=req.task_type,
            goal=req.goal,
            devices=devices,
            steps=steps,
            message=(
                "high-risk steps await human approval"
                if any_high_risk
                else "plan ready for execution"
            ),
        )
        return task


_tasks: dict[str, A2ATask] = {}
_tasks_lock = RLock()
_orchestrator = Orchestrator()


@router.get("/tasks", include_in_schema=False)
def list_tasks(
    principal: Annotated[ApplicationPrincipal, Depends(require_read)],
) -> dict[str, Any]:
    with _tasks_lock:
        tasks = [
            task.model_copy(deep=True)
            for task in _tasks.values()
            if task.tenant_id == principal.tenant_id
        ]
    return {"tasks": tasks}


@router.post("/tasks", status_code=202)
def create_task(
    req: A2ATaskRequest,
    principal: Annotated[ApplicationPrincipal, Depends(require_write)],
) -> A2ATask:
    task = _orchestrator.run(req, principal)
    with _tasks_lock:
        _tasks[task.task_id] = task
    return task


@router.get("/tasks/{task_id}")
def get_task(
    task_id: str, principal: Annotated[ApplicationPrincipal, Depends(require_read)]
) -> A2ATask:
    with _tasks_lock:
        task = _tasks.get(task_id)
        if task is None or task.tenant_id != principal.tenant_id:
            raise HTTPException(status_code=404, detail="task not found")
        return task.model_copy(deep=True)


class TaskDecision(BaseModel):
    decision: str  # approve | reject
    reason: str = ""


@router.post("/tasks/{task_id}/decision")
def decide_task(
    task_id: str,
    d: TaskDecision,
    principal: Annotated[ApplicationPrincipal, Depends(require_write)],
) -> A2ATask:
    with _tasks_lock:
        task = _tasks.get(task_id)
        if task is None or task.tenant_id != principal.tenant_id:
            raise HTTPException(status_code=404, detail="task not found")
        if task.state != TaskState.INPUT_REQUIRED:
            raise HTTPException(status_code=409, detail=f"task is {task.state}, not input-required")
        if d.decision == "approve":
            task.state = TaskState.WORKING
            task.message = "approved; steps dispatched to device executors"
            for step in task.steps:
                if step["approval"] == "required":
                    step["approval"] = "approved"
            task.state = TaskState.COMPLETED
            task.message = "completed"
        elif d.decision == "reject":
            task.state = TaskState.REJECTED
            task.message = d.reason or "rejected by approver"
        else:
            raise HTTPException(status_code=400, detail="decision must be approve or reject")
        return task.model_copy(deep=True)
