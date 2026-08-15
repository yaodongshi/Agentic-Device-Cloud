"""Allow running the eval toolchain as `python -m evals`."""

from evals.cli import main

if __name__ == "__main__":
    raise SystemExit(main())
