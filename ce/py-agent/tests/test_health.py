"""Health probes and V1.0 route boundary tests."""

from __future__ import annotations

import asyncio

import pytest
from app.a2a import PostgresTaskStore
from fastapi.testclient import TestClient


def test_healthz(client: TestClient) -> None:
    response = client.get("/healthz")
    assert response.status_code == 200
    assert response.json() == {"status": "ok"}


def test_readyz(client: TestClient) -> None:
    response = client.get("/readyz")
    assert response.status_code == 200
    assert response.json() == {"status": "ok"}


def test_production_store_fails_closed_without_database_url() -> None:
    with pytest.raises(RuntimeError, match="DATABASE_URL is required"):
        asyncio.run(PostgresTaskStore("").start())


def test_unknown_agent_routes_are_not_registered(client: TestClient) -> None:
    response = client.get("/v2/agents/orchestrations")
    assert response.status_code == 404


@pytest.mark.parametrize("path", ["/docs", "/redoc", "/openapi.json"])
def test_framework_documentation_routes_are_disabled(client: TestClient, path: str) -> None:
    assert client.get(path).status_code == 404
