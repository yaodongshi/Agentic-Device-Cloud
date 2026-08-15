"""CLI entry point tests (python -m evals)."""

from __future__ import annotations

import json
from pathlib import Path

from evals.cli import main


def test_list_suites(suites_dir: Path, capsys) -> None:
    exit_code = main(["--suites-dir", str(suites_dir), "--list-suites"])
    captured = capsys.readouterr()
    assert exit_code == 0
    assert "smoke" in captured.out


def test_run_suite_and_write_report(suites_dir: Path, tmp_path: Path, capsys) -> None:
    output = tmp_path / "report.json"
    exit_code = main(["--suites-dir", str(suites_dir), "--suite", "smoke", "--output", str(output)])
    captured = capsys.readouterr()
    assert exit_code == 0
    assert "pass_rate=100.0%" in captured.out
    report = json.loads(output.read_text(encoding="utf-8"))
    assert report["passed"] == 4
    assert report["failed"] == 0


def test_missing_suite_returns_2(suites_dir: Path, capsys) -> None:
    exit_code = main(["--suites-dir", str(suites_dir), "--suite", "nope"])
    assert exit_code == 2
    assert "not found" in capsys.readouterr().err


def test_tool_filter_removing_all_cases_returns_2(suites_dir: Path, capsys) -> None:
    exit_code = main(["--suites-dir", str(suites_dir), "--suite", "smoke", "--tool-filter", "zzz"])
    assert exit_code == 2
    assert "removed all cases" in capsys.readouterr().err


def test_tool_filter_keeps_matching_cases(suites_dir: Path, capsys) -> None:
    exit_code = main(
        ["--suites-dir", str(suites_dir), "--suite", "smoke", "--tool-filter", "reboot"]
    )
    captured = capsys.readouterr()
    assert exit_code == 0
    assert "total=1" in captured.out
