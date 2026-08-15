"""Eval API endpoint tests (/v2/agents/evals/*)."""

from __future__ import annotations

from fastapi.testclient import TestClient
from tests.conftest import SMOKE_SUITE


def test_create_run_executes_and_returns_report(client: TestClient) -> None:
    response = client.post("/v2/agents/evals/runs", json=SMOKE_SUITE)
    assert response.status_code == 201
    body = response.json()
    assert body["suite"] == "smoke"
    assert body["total"] == 4
    assert body["passed"] == 4
    assert body["failed"] == 0
    assert len(body["results"]) == 4
    assert body["run_id"]


def test_get_run_returns_stored_report(client: TestClient) -> None:
    created = client.post("/v2/agents/evals/runs", json=SMOKE_SUITE).json()
    fetched = client.get(f"/v2/agents/evals/runs/{created['run_id']}")
    assert fetched.status_code == 200
    assert fetched.json()["run_id"] == created["run_id"]
    assert fetched.json()["passed"] == 4


def test_get_unknown_run_404(client: TestClient) -> None:
    response = client.get("/v2/agents/evals/runs/missing-run-id")
    assert response.status_code == 404


def test_invalid_expect_status_rejected_with_422(client: TestClient) -> None:
    payload = {
        "suite": "bad-status",
        "cases": [{"name": "c1", "tool": "dev-001::reboot", "expect_status": "weird"}],
    }
    response = client.post("/v2/agents/evals/runs", json=payload)
    assert response.status_code == 422


def test_empty_cases_rejected_with_422(client: TestClient) -> None:
    response = client.post("/v2/agents/evals/runs", json={"suite": "empty", "cases": []})
    assert response.status_code == 422


def test_list_suites(client: TestClient) -> None:
    response = client.get("/v2/agents/evals/suites")
    assert response.status_code == 200
    assert response.json()["suites"] == ["broken", "smoke"]


def test_get_suite(client: TestClient) -> None:
    response = client.get("/v2/agents/evals/suites/smoke")
    assert response.status_code == 200
    assert response.json()["suite"] == "smoke"
    assert len(response.json()["cases"]) == 4


def test_get_unknown_suite_404(client: TestClient) -> None:
    response = client.get("/v2/agents/evals/suites/nope")
    assert response.status_code == 404
