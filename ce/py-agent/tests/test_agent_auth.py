"""Real-time authentication and route authorization tests for the agent plane."""

from __future__ import annotations

import json
from collections.abc import Callable
from pathlib import Path
from typing import Any

import httpx
import pytest
from app import llm_router
from app.auth import (
    SCOPE_EVALS_READ,
    SCOPE_EVALS_WRITE,
    SCOPE_LLM_INVOKE,
    SCOPE_TASKS_READ,
    SCOPE_TASKS_WRITE,
)
from app.llm import Usage
from app.main import create_app
from fastapi.routing import APIRoute
from fastapi.testclient import TestClient

from tests.task_store import MemoryTaskStore

CredentialState = dict[str, set[str] | bool | str]


def _suites_dir(tmp_path: Path) -> Path:
    (tmp_path / "smoke.json").write_text(
        json.dumps(
            {
                "suite": "smoke",
                "tenant": "forged-tenant",
                "cases": [{"name": "ok", "tool": "device::status"}],
            }
        ),
        encoding="utf-8",
    )
    return tmp_path


def _credential_state(
    scopes: set[str], tenant_id: str = "tenant-a", application_id: str = "app-a"
) -> CredentialState:
    return {
        "active": True,
        "scopes": scopes,
        "tenant_id": tenant_id,
        "application_id": application_id,
    }


def _auth_transport(
    credentials: dict[str, CredentialState], calls: list[tuple[str, str]] | None = None
) -> httpx.MockTransport:
    def backend(request: httpx.Request) -> httpx.Response:
        credential = request.headers.get("X-ADC-Application-Credential", "")
        if not credential and request.headers.get("Authorization", "").startswith("Bearer "):
            credential = request.headers["Authorization"].removeprefix("Bearer ")
        scope = json.loads(request.content)["required_scope"]
        if calls is not None:
            calls.append((credential, scope))
        state = credentials.get(credential)
        if state is None or not state.get("active", False):
            return httpx.Response(401)
        scopes = state["scopes"]
        assert isinstance(scopes, set)
        if scope not in scopes:
            return httpx.Response(403)
        return httpx.Response(
            200,
            json={
                "active": True,
                "tenant_id": state["tenant_id"],
                "application_id": state["application_id"],
                "required_scope": scope,
            },
        )

    return httpx.MockTransport(backend)


def _headers(credential: str) -> dict[str, str]:
    return {"X-ADC-Application-Credential": credential, "X-ADC-Tenant-ID": "forged"}


def _client(transport: httpx.AsyncBaseTransport, suites_dir: Path | None = None) -> TestClient:
    return TestClient(
        create_app(
            suites_dir,
            auth_transport=transport,
            task_store=MemoryTaskStore(),
        )
    )


@pytest.mark.parametrize(
    ("response_factory", "expected"),
    [
        (lambda _request: httpx.Response(401), 401),
        (lambda _request: httpx.Response(403), 403),
        (lambda _request: httpx.Response(503), 503),
        (lambda _request: httpx.Response(200, content=b"not-json"), 503),
        (
            lambda _request: httpx.Response(
                200,
                json={
                    "active": True,
                    "tenant_id": "tenant-a",
                    "application_id": "app-a",
                    "required_scope": "wrong",
                },
            ),
            503,
        ),
    ],
)
def test_introspection_failures_fail_closed(
    response_factory: Callable[[httpx.Request], httpx.Response], expected: int
) -> None:
    api = _client(httpx.MockTransport(response_factory))
    response = api.get("/v2/agents/evals/suites", headers=_headers("credential"))
    assert response.status_code == expected


def test_introspection_timeout_fails_closed() -> None:
    def timeout(request: httpx.Request) -> httpx.Response:
        raise httpx.ReadTimeout("timed out", request=request)

    api = _client(httpx.MockTransport(timeout))
    assert api.get("/v2/agents/evals/suites", headers=_headers("credential")).status_code == 503


@pytest.mark.parametrize(
    ("method", "path", "scope", "body"),
    [
        ("GET", "/v2/agents/evals/suites", SCOPE_EVALS_READ, None),
        ("GET", "/v2/agents/evals/suites/smoke", SCOPE_EVALS_READ, None),
        ("GET", "/v2/agents/evals/runs/missing", SCOPE_EVALS_READ, None),
        (
            "POST",
            "/v2/agents/evals/runs",
            SCOPE_EVALS_WRITE,
            {"suite": "smoke", "cases": [{"name": "ok", "tool": "d::t"}]},
        ),
    ],
)
def test_every_eval_route_enforces_its_scope(
    tmp_path: Path, method: str, path: str, scope: str, body: dict[str, Any] | None
) -> None:
    states = {"allowed": _credential_state({scope}), "wrong": _credential_state(set())}
    api = _client(_auth_transport(states), _suites_dir(tmp_path))
    assert api.request(method, path, json=body).status_code == 401
    assert api.request(method, path, headers=_headers("wrong"), json=body).status_code == 403
    assert api.request(method, path, headers=_headers("allowed"), json=body).status_code in {
        200,
        201,
        404,
    }


def test_eval_reports_are_tenant_and_application_isolated(tmp_path: Path) -> None:
    states = {
        "app-a": _credential_state({SCOPE_EVALS_READ, SCOPE_EVALS_WRITE}),
        "app-b": _credential_state({SCOPE_EVALS_READ, SCOPE_EVALS_WRITE}, application_id="app-b"),
        "tenant-b": _credential_state(
            {SCOPE_EVALS_READ}, tenant_id="tenant-b", application_id="app-b"
        ),
    }
    api = _client(_auth_transport(states), _suites_dir(tmp_path))
    created = api.post(
        "/v2/agents/evals/runs",
        headers=_headers("app-a"),
        json={
            "suite": "smoke",
            "tenant": "forged",
            "cases": [{"name": "ok", "tool": "d::t"}],
        },
    )
    assert created.status_code == 201
    path = f"/v2/agents/evals/runs/{created.json()['run_id']}"
    assert api.get(path, headers=_headers("app-a")).status_code == 200
    assert api.get(path, headers=_headers("app-b")).status_code == 404
    assert api.get(path, headers=_headers("tenant-b")).status_code == 404


class FakeLLMRouter:
    primary = type("Provider", (), {"name": "primary", "model": "test"})()
    fallback = None

    async def complete(self, _messages: list[dict[str, Any]]) -> tuple[str, Usage, str]:
        return "ok", Usage(2, 3), "primary"


class RecordingMeter:
    def __init__(self) -> None:
        self.calls: list[tuple[str, str, str, int]] = []

    def record(self, tenant_id: str, application_id: str, source: str, tokens: int) -> None:
        self.calls.append((tenant_id, application_id, source, tokens))


@pytest.mark.parametrize(
    ("method", "path", "body"),
    [
        ("GET", "/v2/agents/llm/models", None),
        ("GET", "/v2/agents/llm/usage?tenant_id=forged", None),
        (
            "POST",
            "/v2/agents/llm/chat/completions",
            {"tenant_id": "forged", "messages": [{"role": "user", "content": "hi"}]},
        ),
    ],
)
def test_every_llm_route_requires_invoke_scope(
    monkeypatch: pytest.MonkeyPatch, method: str, path: str, body: dict[str, Any] | None
) -> None:
    monkeypatch.setattr(llm_router, "_router", FakeLLMRouter())
    states = {
        "allowed": _credential_state({SCOPE_LLM_INVOKE}),
        "wrong": _credential_state(set()),
    }
    api = _client(_auth_transport(states))
    assert api.request(method, path, json=body).status_code == 401
    assert api.request(method, path, headers=_headers("wrong"), json=body).status_code == 403
    assert api.request(method, path, headers=_headers("allowed"), json=body).status_code == 200


def test_llm_ignores_client_tenant_and_meters_authenticated_principal(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    meter = RecordingMeter()
    monkeypatch.setattr(llm_router, "_router", FakeLLMRouter())
    monkeypatch.setattr(llm_router, "_meter", meter)
    states = {
        "allowed": _credential_state(
            {SCOPE_LLM_INVOKE}, tenant_id="real-tenant", application_id="real-app"
        )
    }
    api = _client(_auth_transport(states))
    response = api.post(
        "/v2/agents/llm/chat/completions",
        headers=_headers("allowed"),
        json={"tenant_id": "forged", "messages": [{"role": "user", "content": "hi"}]},
    )
    assert response.status_code == 200
    assert meter.calls == [("real-tenant", "real-app", "primary", 5)]
    usage = api.get("/v2/agents/llm/usage?tenant_id=forged", headers=_headers("allowed"))
    assert usage.json()["tenant_id"] == "real-tenant"
    assert usage.json()["application_id"] == "real-app"


def test_public_routes_and_agent_route_enumeration() -> None:
    calls: list[tuple[str, str]] = []
    states = {
        "all": _credential_state(
            {
                SCOPE_TASKS_READ,
                SCOPE_TASKS_WRITE,
                SCOPE_LLM_INVOKE,
                SCOPE_EVALS_READ,
                SCOPE_EVALS_WRITE,
            }
        )
    }
    app = create_app(auth_transport=_auth_transport(states, calls), task_store=MemoryTaskStore())
    with TestClient(app) as api:
        for path in ("/healthz", "/readyz", "/.well-known/agent-card.json"):
            assert api.get(path).status_code == 200
    assert calls == []

    expected = {
        ("GET", "/v2/agents/a2a/tasks"),
        ("POST", "/v2/agents/a2a/tasks"),
        ("GET", "/v2/agents/a2a/tasks/{task_id}"),
        ("POST", "/v2/agents/a2a/tasks/{task_id}/decision"),
        ("GET", "/v2/agents/llm/models"),
        ("POST", "/v2/agents/llm/chat/completions"),
        ("GET", "/v2/agents/llm/usage"),
        ("GET", "/v2/agents/evals/suites"),
        ("GET", "/v2/agents/evals/suites/{name}"),
        ("POST", "/v2/agents/evals/runs"),
        ("GET", "/v2/agents/evals/runs/{run_id}"),
    }
    routes = [route for route in app.routes if isinstance(route, APIRoute)]
    for included in app.routes:
        original_router = getattr(included, "original_router", None)
        if original_router is not None:
            routes.extend(route for route in original_router.routes if isinstance(route, APIRoute))
    actual = {
        (method, route.path)
        for route in routes
        if route.path.startswith("/v2/agents/")
        for method in route.methods
    }
    assert actual == expected


@pytest.mark.parametrize("action", ["disable", "rotate", "revoke"])
def test_credential_changes_take_effect_on_next_request_without_cache(action: str) -> None:
    calls: list[tuple[str, str]] = []
    states = {"old": _credential_state({SCOPE_EVALS_READ})}
    api = _client(_auth_transport(states, calls))
    assert api.get("/v2/agents/evals/suites", headers=_headers("old")).status_code == 200

    states["old"]["active"] = False
    if action == "rotate":
        states["new"] = _credential_state({SCOPE_EVALS_READ})
    assert api.get("/v2/agents/evals/suites", headers=_headers("old")).status_code == 401
    if action == "rotate":
        assert api.get("/v2/agents/evals/suites", headers=_headers("new")).status_code == 200
    assert len(calls) == (3 if action == "rotate" else 2)
