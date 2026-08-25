"""Tests for the A2A surface and deterministic pipeline (B7)."""

from __future__ import annotations

from fastapi import Request
from fastapi.testclient import TestClient

from app.a2a import ApplicationPrincipal, Orchestrator, build_agent_card
from app.main import create_app

PRINCIPAL = ApplicationPrincipal(
    active=True,
    application_id="app-a",
    tenant_id="tenant-a",
    required_scope="a2a.tasks:write",
)


def test_agent_card_shape() -> None:
    card = build_agent_card()
    assert card.name == "ADC Device Orchestrator"
    assert len(card.skills) == 4
    assert {s["id"] for s in card.skills} == {"diagnose", "schedule", "inspect", "maintain"}


def test_orchestrator_high_risk_requires_input() -> None:
    o = Orchestrator()
    task = o.run(
        __import__("app.a2a", fromlist=["A2ATaskRequest"]).A2ATaskRequest(
            task_type="maintain", goal="grease the spindle", devices=["cnc-01"]
        ),
        PRINCIPAL,
    )
    assert task.state.value == "input-required"
    assert task.steps[0]["approval"] == "required"
    assert task.steps[0]["risk_level"] == 2


def test_orchestrator_low_risk_working() -> None:
    o = Orchestrator()
    req = __import__("app.a2a", fromlist=["A2ATaskRequest"]).A2ATaskRequest(
        task_type="inspect", goal="read status", devices=["agv-01"]
    )
    task = o.run(req, PRINCIPAL)
    assert task.state.value == "working"
    assert task.steps[0]["approval"] == "none"


def test_orchestrator_rejects_bad_type() -> None:
    o = Orchestrator()
    req = __import__("app.a2a", fromlist=["A2ATaskRequest"]).A2ATaskRequest(
        task_type="nuke", goal="x", devices=["d"]
    )
    try:
        o.run(req, PRINCIPAL)
        raise AssertionError("expected HTTPException")
    except Exception as e:  # noqa: BLE001
        assert getattr(e, "status_code", None) == 400


def test_endpoints_flow() -> None:
    async def allow(_request: Request, scope: str) -> ApplicationPrincipal:
        return ApplicationPrincipal(
            active=True,
            application_id="app-a",
            tenant_id="tenant-a",
            required_scope=scope,
        )

    client = TestClient(create_app(introspector=allow))
    r = client.post(
        "/v2/agents/a2a/tasks",
        json={"task_type": "maintain", "goal": "change tool", "devices": ["cnc-01"]},
    )
    assert r.status_code == 202
    task = r.json()
    assert task["state"] == "input-required"
    r2 = client.get(f"/v2/agents/a2a/tasks/{task['task_id']}")
    assert r2.status_code == 200
    r3 = client.post(
        f"/v2/agents/a2a/tasks/{task['task_id']}/decision", json={"decision": "approve"}
    )
    assert r3.status_code == 200
    assert r3.json()["state"] == "completed"
    # second decision conflicts
    r4 = client.post(
        f"/v2/agents/a2a/tasks/{task['task_id']}/decision", json={"decision": "approve"}
    )
    assert r4.status_code == 409
