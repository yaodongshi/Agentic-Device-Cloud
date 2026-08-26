"""Authentication, scope, and tenant isolation tests for the public A2A API."""

from __future__ import annotations

import httpx
from app.auth import SCOPE_TASKS_READ, SCOPE_TASKS_WRITE
from app.main import create_app
from fastapi.testclient import TestClient

from tests.task_store import MemoryTaskStore

revoked_credentials: set[str] = set()


def auth_backend(request: httpx.Request) -> httpx.Response:
    credential = request.headers.get("X-ADC-Application-Credential", "")
    if not credential and request.headers.get("Authorization", "").startswith("Bearer "):
        credential = request.headers["Authorization"].removeprefix("Bearer ")
    scope = __import__("json").loads(request.content)["required_scope"]
    if not credential or credential in revoked_credentials:
        return httpx.Response(401)
    allowed = {
        "tenant-a-read": {SCOPE_TASKS_READ},
        "tenant-a-write": {SCOPE_TASKS_WRITE},
        "tenant-a-both": {SCOPE_TASKS_READ, SCOPE_TASKS_WRITE},
        "tenant-b-both": {SCOPE_TASKS_READ, SCOPE_TASKS_WRITE},
    }
    if scope not in allowed.get(credential, set()):
        return httpx.Response(403)
    tenant = "tenant-b" if credential.startswith("tenant-b") else "tenant-a"
    return httpx.Response(
        200,
        json={
            "active": True,
            "application_id": credential,
            "tenant_id": tenant,
            "required_scope": scope,
        },
    )


def client() -> TestClient:
    revoked_credentials.clear()
    return TestClient(
        create_app(auth_transport=httpx.MockTransport(auth_backend), task_store=MemoryTaskStore())
    )


def headers(credential: str) -> dict[str, str]:
    return {
        "X-ADC-Application-Credential": credential,
        "Idempotency-Key": credential + "-key",
    }


def task_payload(**extra: str) -> dict[str, object]:
    return {
        "task_type": "maintain",
        "goal": "change tool",
        "devices": ["cnc-01"],
        **extra,
    }


def test_agent_card_remains_public() -> None:
    response = client().get("/.well-known/agent-card.json")
    assert response.status_code == 200


def test_all_a2a_routes_reject_anonymous_callers() -> None:
    api = client()
    assert api.get("/v2/agents/a2a/tasks").status_code == 401
    assert api.post("/v2/agents/a2a/tasks", json=task_payload()).status_code == 401
    assert api.get("/v2/agents/a2a/tasks/missing").status_code == 401
    assert (
        api.post("/v2/agents/a2a/tasks/missing/decision", json={"decision": "approve"}).status_code
        == 401
    )


def test_read_and_write_scopes_are_enforced() -> None:
    api = client()
    assert api.get("/v2/agents/a2a/tasks", headers=headers("tenant-a-read")).status_code == 200
    assert (
        api.post(
            "/v2/agents/a2a/tasks", headers=headers("tenant-a-read"), json=task_payload()
        ).status_code
        == 403
    )


def test_create_requires_idempotency_key_and_replays_same_request() -> None:
    api = client()
    credential_only = {"X-ADC-Application-Credential": "tenant-a-both"}
    assert (
        api.post("/v2/agents/a2a/tasks", headers=credential_only, json=task_payload()).status_code
        == 400
    )
    first = api.post("/v2/agents/a2a/tasks", headers=headers("tenant-a-both"), json=task_payload())
    replay = api.post("/v2/agents/a2a/tasks", headers=headers("tenant-a-both"), json=task_payload())
    assert replay.status_code == 202
    assert replay.json()["task_id"] == first.json()["task_id"]
    conflict = api.post(
        "/v2/agents/a2a/tasks",
        headers=headers("tenant-a-both"),
        json=task_payload(goal="different goal"),
    )
    assert conflict.status_code == 409


def test_machine_decision_is_always_forbidden() -> None:
    api = client()
    first = api.post(
        "/v2/agents/a2a/tasks", headers=headers("tenant-a-both"), json=task_payload()
    ).json()
    decision_url = f"/v2/agents/a2a/tasks/{first['task_id']}/decision"
    assert (
        api.post(
            decision_url, headers=headers("tenant-a-read"), json={"decision": "approve"}
        ).status_code
        == 403
    )
    approved = api.post(
        decision_url, headers=headers("tenant-a-both"), json={"decision": "approve"}
    )
    assert approved.status_code == 403


def test_bearer_credential_is_forwarded_to_introspection() -> None:
    api = client()
    response = api.get("/v2/agents/a2a/tasks", headers={"Authorization": "Bearer tenant-a-read"})
    assert response.status_code == 200
    created = api.post(
        "/v2/agents/a2a/tasks", headers=headers("tenant-a-write"), json=task_payload()
    )
    assert created.status_code == 202
    assert (
        api.get(
            f"/v2/agents/a2a/tasks/{created.json()['task_id']}",
            headers=headers("tenant-a-write"),
        ).status_code
        == 403
    )


def test_tenant_and_application_are_server_injected_and_isolated() -> None:
    api = client()
    created = api.post(
        "/v2/agents/a2a/tasks",
        headers=headers("tenant-a-both"),
        json=task_payload(tenant_id="tenant-b"),
    )
    assert created.status_code == 202
    task = created.json()
    assert task["tenant_id"] == "tenant-a"
    assert task["application_id"] == "tenant-a-both"
    task_url = f"/v2/agents/a2a/tasks/{task['task_id']}"
    assert api.get(task_url, headers=headers("tenant-b-both")).status_code == 404
    assert (
        api.post(
            task_url + "/decision",
            headers=headers("tenant-b-both"),
            json={"decision": "reject"},
        ).status_code
        == 403
    )
    listed = api.get("/v2/agents/a2a/tasks", headers=headers("tenant-b-both"))
    assert listed.status_code == 200
    assert all(item["tenant_id"] == "tenant-b" for item in listed.json()["tasks"])


def test_revocation_is_checked_on_every_request() -> None:
    api = client()
    created = api.post(
        "/v2/agents/a2a/tasks", headers=headers("tenant-a-both"), json=task_payload()
    )
    assert created.status_code == 202
    assert api.get("/v2/agents/a2a/tasks", headers=headers("tenant-a-both")).status_code == 200
    revoked_credentials.add("tenant-a-both")
    assert api.get("/v2/agents/a2a/tasks", headers=headers("tenant-a-both")).status_code == 401


def test_introspection_failure_fails_closed() -> None:
    def unavailable(_request: httpx.Request) -> httpx.Response:
        return httpx.Response(500)

    api = TestClient(
        create_app(auth_transport=httpx.MockTransport(unavailable), task_store=MemoryTaskStore())
    )
    assert api.get("/v2/agents/a2a/tasks", headers=headers("tenant-a-both")).status_code == 503


def test_inactive_introspection_response_fails_closed() -> None:
    def inactive(_request: httpx.Request) -> httpx.Response:
        return httpx.Response(
            200,
            json={
                "active": False,
                "application_id": "app-a",
                "tenant_id": "tenant-a",
                "required_scope": SCOPE_TASKS_READ,
            },
        )

    api = TestClient(
        create_app(auth_transport=httpx.MockTransport(inactive), task_store=MemoryTaskStore())
    )
    assert api.get("/v2/agents/a2a/tasks", headers=headers("tenant-a-read")).status_code == 503
