"""Suite loading and input validation failure tests."""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from evals import loader
from pydantic import ValidationError


def test_load_valid_suite(suites_dir: Path) -> None:
    spec = loader.load_suite(suites_dir, "smoke")
    assert spec.suite == "smoke"
    assert spec.tenant == "demo"
    assert len(spec.cases) == 4
    assert spec.cases[0].expect_status == "ok"


def test_missing_suite_file(suites_dir: Path) -> None:
    with pytest.raises(FileNotFoundError):
        loader.load_suite(suites_dir, "nope")


def test_broken_json_is_rejected(suites_dir: Path) -> None:
    with pytest.raises(ValueError):
        loader.load_suite(suites_dir, "broken")


def test_non_object_suite_is_rejected(tmp_path: Path) -> None:
    (tmp_path / "list.json").write_text("[1, 2, 3]", encoding="utf-8")
    with pytest.raises(ValueError, match="JSON object"):
        loader.load_suite(tmp_path, "list")


def test_empty_cases_rejected(tmp_path: Path) -> None:
    (tmp_path / "empty.json").write_text(json.dumps({"suite": "empty", "cases": []}))
    with pytest.raises(ValidationError):
        loader.load_suite(tmp_path, "empty")


def test_invalid_expect_status_rejected(tmp_path: Path) -> None:
    payload = {
        "suite": "bad-status",
        "cases": [{"name": "c1", "tool": "dev-001::reboot", "expect_status": "weird"}],
    }
    (tmp_path / "bad.json").write_text(json.dumps(payload), encoding="utf-8")
    with pytest.raises(ValidationError):
        loader.load_suite(tmp_path, "bad")


def test_oversized_suite_name_rejected(tmp_path: Path) -> None:
    payload = {"suite": "x" * 129, "cases": [{"name": "c1", "tool": "dev-001::reboot"}]}
    (tmp_path / "long.json").write_text(json.dumps(payload), encoding="utf-8")
    with pytest.raises(ValidationError):
        loader.load_suite(tmp_path, "long")


def test_more_than_500_cases_rejected(tmp_path: Path) -> None:
    payload = {
        "suite": "too-many",
        "cases": [{"name": f"c{i}", "tool": "dev-001::reboot"} for i in range(501)],
    }
    (tmp_path / "many.json").write_text(json.dumps(payload), encoding="utf-8")
    with pytest.raises(ValidationError):
        loader.load_suite(tmp_path, "many")


def test_list_suites_is_sorted(suites_dir: Path) -> None:
    assert loader.list_suites(suites_dir) == ["broken", "smoke"]


def test_load_suite_json_accepts_500_cases(tmp_path: Path) -> None:
    payload = {
        "suite": "max",
        "cases": [{"name": f"c{i}", "tool": "dev-001::reboot"} for i in range(500)],
    }
    (tmp_path / "max.json").write_text(json.dumps(payload), encoding="utf-8")
    assert loader.load_suite(tmp_path, "max") is not None
