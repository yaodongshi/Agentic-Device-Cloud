"""Shared fixtures for py-agent tests."""

from __future__ import annotations

import json
from collections.abc import Iterator
from pathlib import Path

import pytest
from app.main import create_app
from evals.harness import EvalHarness
from fastapi.testclient import TestClient

SMOKE_SUITE: dict = {
    "suite": "smoke",
    "tenant": "demo",
    "cases": [
        {"name": "reboot ok", "tool": "dev-001::reboot", "args": {"delay_ms": 500}},
        {"name": "telemetry ok", "tool": "dev-001::get_telemetry"},
        {"name": "set mode blocked", "tool": "dev-002::set_mode", "expect_status": "blocked"},
        {"name": "missing tool errors", "tool": "dev-003::ghost", "expect_status": "error"},
    ],
}


@pytest.fixture
def suites_dir(tmp_path: Path) -> Path:
    (tmp_path / "smoke.json").write_text(json.dumps(SMOKE_SUITE), encoding="utf-8")
    (tmp_path / "broken.json").write_text("{not json", encoding="utf-8")
    return tmp_path


@pytest.fixture
def harness(suites_dir: Path) -> EvalHarness:
    return EvalHarness(suites_dir)


@pytest.fixture
def client(suites_dir: Path) -> Iterator[TestClient]:
    with TestClient(create_app(suites_dir)) as test_client:
        yield test_client
