"""FastAPI assembly for the ADC Python agent plane.

V1.0 exposes only the eval-harness subset under /v2/agents/evals/* plus
health/readiness probes; every other /v2/agents/* route answers 503
(LLD 3.6.1 version boundary).
"""

from __future__ import annotations

import os
from pathlib import Path

import httpx
from evals.harness import EvalHarness
from evals.models import EvalReport, EvalRunSpec
from fastapi import FastAPI, HTTPException
from fastapi.responses import JSONResponse

from app.a2a import (
    DEFAULT_ADMIN_INTERNAL_URL,
    Introspector,
    build_agent_card,
    introspect_application,
)
from app.a2a import router as a2a_router
from app.llm_router import router as llm_router

DEFAULT_SUITES_DIR_ENV = "ADC_EVAL_SUITES_DIR"


def create_app(
    suites_dir: Path | None = None,
    *,
    admin_internal_url: str | None = None,
    introspector: Introspector = introspect_application,
    auth_transport: httpx.AsyncBaseTransport | None = None,
) -> FastAPI:
    """Assemble the application with its own harness instance."""
    harness = EvalHarness(suites_dir or Path(os.environ.get(DEFAULT_SUITES_DIR_ENV, "suites")))
    app = FastAPI(title="ADC Python Agent Plane", version="0.1.0")
    app.state.admin_internal_url = admin_internal_url or os.environ.get(
        "ADC_ADMIN_INTERNAL_URL", DEFAULT_ADMIN_INTERNAL_URL
    )
    app.state.introspector = introspector
    app.state.auth_transport = auth_transport
    app.include_router(llm_router)
    app.include_router(a2a_router)

    @app.get("/.well-known/agent-card.json")
    def agent_card() -> dict[str, object]:
        return build_agent_card().model_dump()

    @app.get("/healthz")
    def healthz() -> dict[str, str]:
        return {"status": "ok"}

    @app.get("/readyz")
    def readyz() -> dict[str, str]:
        return {"status": "ok"}

    @app.get("/v2/agents/evals/suites")
    def list_suites() -> dict[str, list[str]]:
        return {"suites": harness.list_suites()}

    @app.get("/v2/agents/evals/suites/{name}", response_model=EvalRunSpec)
    def get_suite(name: str) -> EvalRunSpec:
        try:
            return harness.load_suite(name)
        except FileNotFoundError as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from exc

    @app.post("/v2/agents/evals/runs", response_model=EvalReport, status_code=201)
    def create_run(spec: EvalRunSpec) -> EvalReport:
        return harness.run(spec)

    @app.get("/v2/agents/evals/runs/{run_id}", response_model=EvalReport)
    def get_run(run_id: str) -> EvalReport:
        report = harness.report(run_id)
        if report is None:
            raise HTTPException(status_code=404, detail=f"run not found: {run_id}")
        return report

    @app.api_route("/v2/agents/{path:path}", methods=["GET", "POST", "PUT", "PATCH", "DELETE"])
    def agent_plane_not_ready(path: str) -> JSONResponse:
        return JSONResponse(
            status_code=503,
            content={"detail": f"agent plane endpoint not available in V1.0: /v2/agents/{path}"},
        )

    return app


app = create_app()


if __name__ == "__main__":
    import uvicorn

    uvicorn.run("app.main:app", host="0.0.0.0", port=8000)
