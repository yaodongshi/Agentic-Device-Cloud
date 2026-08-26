"""A2A HTTP surface and PostgreSQL-backed durable task state."""

from __future__ import annotations

import hashlib
import json
import re
import uuid
from datetime import datetime
from enum import StrEnum
from typing import Annotated, Any, Protocol

import asyncpg
from fastapi import APIRouter, Depends, Header, HTTPException, Request
from pydantic import BaseModel, Field

from app.auth import ApplicationPrincipal, require_tasks_read, require_tasks_write

router = APIRouter(prefix="/v2/agents/a2a", tags=["a2a"])

AGENT_NAME = "ADC Device Orchestrator"
AGENT_DESCRIPTION_EN = "Orchestrates physical devices (CNC/PLC/AGV) behind HITL approval."
AGENT_DESCRIPTION_ZH = "在 HITL 审批约束下编排物理设备（CNC/PLC/AGV）。"
TASK_TYPES = ["diagnose", "schedule", "inspect", "maintain"]
IDEMPOTENCY_KEY_RE = re.compile(r"^[A-Za-z0-9._:-]{1,128}$")


class TaskState(StrEnum):
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
    version: int = 1
    approver_user_id: str | None = None
    approver_identity: str | None = None
    decision_reason: str | None = None
    created_at: datetime | None = None
    updated_at: datetime | None = None
    decided_at: datetime | None = None


class TaskNotFoundError(Exception):
    pass


class IdempotencyConflictError(Exception):
    pass


class VersionConflictError(Exception):
    pass


class TaskStore(Protocol):
    async def start(self) -> None: ...
    async def close(self) -> None: ...
    async def ready(self) -> bool: ...
    async def create(self, task: A2ATask, idempotency_key: str, request_hash: str) -> A2ATask: ...
    async def list(self, tenant_id: str) -> list[A2ATask]: ...
    async def get(self, tenant_id: str, task_id: str) -> A2ATask: ...
    async def transition(
        self,
        tenant_id: str,
        task_id: str,
        expected_state: TaskState,
        expected_version: int,
        new_state: TaskState,
        message: str,
    ) -> A2ATask: ...


_TASK_COLUMNS = """task_id::text, tenant_id::text, application_id::text, state,
task_type, goal, devices, steps, message, version, approver_user_id::text,
approver_identity, decision_reason, created_at, updated_at, decided_at"""


def _task_from_record(row: asyncpg.Record) -> A2ATask:
    return A2ATask(
        task_id=row["task_id"],
        tenant_id=row["tenant_id"],
        application_id=row["application_id"],
        state=row["state"],
        task_type=row["task_type"],
        goal=row["goal"],
        devices=json.loads(row["devices"]),
        steps=json.loads(row["steps"]),
        message=row["message"],
        version=row["version"],
        approver_user_id=row["approver_user_id"],
        approver_identity=row["approver_identity"],
        decision_reason=row["decision_reason"],
        created_at=row["created_at"],
        updated_at=row["updated_at"],
        decided_at=row["decided_at"],
    )


class PostgresTaskStore:
    """Direct async PostgreSQL repository; no in-memory production fallback."""

    def __init__(self, dsn: str) -> None:
        self._dsn = dsn.strip()
        self._pool: asyncpg.Pool | None = None

    async def start(self) -> None:
        if not self._dsn:
            raise RuntimeError("DATABASE_URL is required for durable A2A tasks")
        self._pool = await asyncpg.create_pool(self._dsn, min_size=1, max_size=10)
        if not await self.ready():
            await self.close()
            raise RuntimeError("adc_a2a_tasks is unavailable")

    async def close(self) -> None:
        if self._pool is not None:
            await self._pool.close()
            self._pool = None

    async def ready(self) -> bool:
        if self._pool is None:
            return False
        try:
            return bool(
                await self._pool.fetchval("SELECT to_regclass('adc_a2a_tasks') IS NOT NULL")
            )
        except asyncpg.PostgresError, OSError:
            return False

    def _require_pool(self) -> asyncpg.Pool:
        if self._pool is None:
            raise RuntimeError("A2A task store is not started")
        return self._pool

    async def create(self, task: A2ATask, idempotency_key: str, request_hash: str) -> A2ATask:
        pool = self._require_pool()
        row = await pool.fetchrow(
            f"""INSERT INTO adc_a2a_tasks
            (task_id,tenant_id,application_id,idempotency_key,request_hash,state,task_type,goal,devices,steps,message)
            VALUES($1::uuid,$2::uuid,$3::uuid,$4,$5,$6,$7,$8,$9::jsonb,$10::jsonb,$11)
            ON CONFLICT (tenant_id,application_id,idempotency_key) DO NOTHING
            RETURNING {_TASK_COLUMNS}""",
            task.task_id,
            task.tenant_id,
            task.application_id,
            idempotency_key,
            request_hash,
            task.state.value,
            task.task_type,
            task.goal,
            json.dumps(task.devices),
            json.dumps(task.steps),
            task.message,
        )
        if row is not None:
            return _task_from_record(row)
        row = await pool.fetchrow(
            f"""SELECT {_TASK_COLUMNS}, request_hash FROM adc_a2a_tasks
            WHERE tenant_id=$1::uuid AND application_id=$2::uuid AND idempotency_key=$3""",
            task.tenant_id,
            task.application_id,
            idempotency_key,
        )
        if row is None or row["request_hash"] != request_hash:
            raise IdempotencyConflictError
        return _task_from_record(row)

    async def list(self, tenant_id: str) -> list[A2ATask]:
        rows = await self._require_pool().fetch(
            f"""SELECT {_TASK_COLUMNS} FROM adc_a2a_tasks
            WHERE tenant_id=$1::uuid ORDER BY created_at DESC,task_id DESC""",
            tenant_id,
        )
        return [_task_from_record(row) for row in rows]

    async def get(self, tenant_id: str, task_id: str) -> A2ATask:
        row = await self._require_pool().fetchrow(
            f"""SELECT {_TASK_COLUMNS} FROM adc_a2a_tasks
            WHERE tenant_id=$1::uuid AND task_id=$2::uuid""",
            tenant_id,
            task_id,
        )
        if row is None:
            raise TaskNotFoundError
        return _task_from_record(row)

    async def transition(
        self,
        tenant_id: str,
        task_id: str,
        expected_state: TaskState,
        expected_version: int,
        new_state: TaskState,
        message: str,
    ) -> A2ATask:
        row = await self._require_pool().fetchrow(
            f"""UPDATE adc_a2a_tasks SET state=$5,message=$6,version=version+1,updated_at=now()
            WHERE tenant_id=$1::uuid AND task_id=$2::uuid AND state=$3 AND version=$4
            RETURNING {_TASK_COLUMNS}""",
            tenant_id,
            task_id,
            expected_state.value,
            expected_version,
            new_state.value,
            message,
        )
        if row is not None:
            return _task_from_record(row)
        if not await self._require_pool().fetchval(
            """SELECT EXISTS(SELECT 1 FROM adc_a2a_tasks
            WHERE tenant_id=$1::uuid AND task_id=$2::uuid)""",
            tenant_id,
            task_id,
        ):
            raise TaskNotFoundError
        raise VersionConflictError


async def require_read(request: Request) -> ApplicationPrincipal:
    return await require_tasks_read(request)


async def require_write(request: Request) -> ApplicationPrincipal:
    return await require_tasks_write(request)


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
    HIGH_RISK_KEYWORDS = ("set", "write", "move", "reboot", "stop", "override")

    def run(self, req: A2ATaskRequest, principal: ApplicationPrincipal) -> A2ATask:
        if req.task_type not in TASK_TYPES:
            raise HTTPException(status_code=400, detail=f"unsupported task_type {req.task_type}")
        devices = list(dict.fromkeys(device for device in req.devices if device))
        if not devices:
            raise HTTPException(status_code=400, detail="devices must not be empty")
        steps: list[dict[str, Any]] = []
        any_high_risk = False
        for device in devices:
            tool = f"{req.task_type}_action"
            risk = (
                2
                if any(key in tool for key in self.HIGH_RISK_KEYWORDS)
                or req.task_type == "maintain"
                else 1
            )
            any_high_risk = any_high_risk or risk >= 2
            steps.append(
                {
                    "agent": "executor",
                    "device": device,
                    "tool": tool,
                    "risk_level": risk,
                    "approval": "required" if risk >= 2 else "none",
                }
            )
        return A2ATask(
            task_id=str(uuid.uuid4()),
            tenant_id=principal.tenant_id,
            application_id=principal.application_id,
            state=TaskState.INPUT_REQUIRED if any_high_risk else TaskState.WORKING,
            task_type=req.task_type,
            goal=req.goal,
            devices=devices,
            steps=steps,
            message="high-risk steps await human approval"
            if any_high_risk
            else "plan ready for execution",
        )


def canonical_request_hash(req: A2ATaskRequest, task: A2ATask) -> str:
    raw = json.dumps(
        {"task_type": req.task_type, "goal": req.goal, "devices": task.devices},
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode()
    return hashlib.sha256(raw).hexdigest()


def _store(request: Request) -> TaskStore:
    return request.app.state.task_store


@router.get("/tasks", include_in_schema=False)
async def list_tasks(
    principal: Annotated[ApplicationPrincipal, Depends(require_read)], request: Request
) -> dict[str, Any]:
    return {"tasks": await _store(request).list(principal.tenant_id)}


@router.post("/tasks", status_code=202)
async def create_task(
    req: A2ATaskRequest,
    request: Request,
    principal: Annotated[ApplicationPrincipal, Depends(require_write)],
    idempotency_key: Annotated[str | None, Header(alias="Idempotency-Key")] = None,
) -> A2ATask:
    if idempotency_key is None or not IDEMPOTENCY_KEY_RE.fullmatch(idempotency_key):
        raise HTTPException(status_code=400, detail="valid Idempotency-Key header is required")
    task = Orchestrator().run(req, principal)
    try:
        return await _store(request).create(
            task, idempotency_key, canonical_request_hash(req, task)
        )
    except IdempotencyConflictError as exc:
        raise HTTPException(
            status_code=409, detail="idempotency key reused for different request"
        ) from exc


@router.get("/tasks/{task_id}")
async def get_task(
    task_id: str,
    request: Request,
    principal: Annotated[ApplicationPrincipal, Depends(require_read)],
) -> A2ATask:
    try:
        return await _store(request).get(principal.tenant_id, task_id)
    except TaskNotFoundError as exc:
        raise HTTPException(status_code=404, detail="task not found") from exc


class TaskDecision(BaseModel):
    decision: str
    reason: str = ""


@router.post("/tasks/{task_id}/decision")
async def decide_task(
    task_id: str,
    decision: TaskDecision,
    principal: Annotated[ApplicationPrincipal, Depends(require_write)],
) -> None:
    del task_id, decision, principal
    raise HTTPException(status_code=403, detail="human session approval required")
