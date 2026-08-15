"""Eval suite loading from JSON files (suites/ directory convention, LLD 3.6.4)."""

from __future__ import annotations

import json
from pathlib import Path

from evals.models import EvalRunSpec


def load_suite_json(path: Path) -> EvalRunSpec:
    """Load and validate a single eval suite JSON file."""
    with path.open(encoding="utf-8") as suite_file:
        raw = json.load(suite_file)
    if not isinstance(raw, dict):
        raise ValueError(f"suite file {path} must contain a JSON object")
    return EvalRunSpec.model_validate(raw)


def load_suite(suites_dir: Path, name: str) -> EvalRunSpec:
    """Load a suite by name from a suites directory (looks for <name>.json)."""
    path = suites_dir / f"{name}.json"
    if not path.is_file():
        raise FileNotFoundError(f"suite '{name}' not found in {suites_dir}")
    return load_suite_json(path)


def list_suites(suites_dir: Path) -> list[str]:
    """List suite names available in a suites directory (sorted)."""
    if not suites_dir.is_dir():
        return []
    return sorted(path.stem for path in suites_dir.glob("*.json"))
