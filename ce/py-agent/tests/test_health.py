"""Health probes and V1.0 route boundary tests."""

from __future__ import annotations

from fastapi.testclient import TestClient


def test_healthz(client: TestClient) -> None:
    response = client.get("/healthz")
    assert response.status_code == 200
    assert response.json() == {"status": "ok"}


def test_readyz(client: TestClient) -> None:
    response = client.get("/readyz")
    assert response.status_code == 200
    assert response.json() == {"status": "ok"}


def test_non_eval_agent_routes_answer_503(client: TestClient) -> None:
    response = client.get("/v2/agents/orchestrations")
    assert response.status_code == 503
    assert "not available in V1.0" in response.json()["detail"]
