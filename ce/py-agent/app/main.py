"""FastAPI assembly for the authenticated ADC Python agent plane."""

from __future__ import annotations

import os
from collections.abc import AsyncIterator
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Annotated

import httpx
from evals.harness import EvalHarness
from evals.models import EvalReport, EvalRunSpec
from fastapi import Depends, FastAPI, HTTPException

from app.a2a import PostgresTaskStore, TaskStore, build_agent_card
from app.a2a import router as a2a_router
from app.auth import (
    DEFAULT_ADMIN_INTERNAL_URL,
    ApplicationPrincipal,
    Introspector,
    introspect_application,
    require_evals_read,
    require_evals_write,
)
from app.llm_router import router as llm_router

DEFAULT_SUITES_DIR_ENV = "ADC_EVAL_SUITES_DIR"


def create_app(
    suites_dir: Path | None = None,
    *,
    admin_internal_url: str | None = None,
    introspector: Introspector = introspect_application,
    auth_transport: httpx.AsyncBaseTransport | None = None,
    task_store: TaskStore | None = None,
) -> FastAPI:
    """Assemble the application with its own harness instance."""
    harness = EvalHarness(suites_dir or Path(os.environ.get(DEFAULT_SUITES_DIR_ENV, "suites")))
    store = (
        task_store
        if task_store is not None
        else PostgresTaskStore(os.environ.get("DATABASE_URL", ""))
    )

    @asynccontextmanager
    async def lifespan(_app: FastAPI) -> AsyncIterator[None]:
        await store.start()
        try:
            yield
        finally:
            await store.close()

    app = FastAPI(
        title="ADC Python Agent Plane",
        version="0.1.0",
        lifespan=lifespan,
        docs_url=None,
        redoc_url=None,
        openapi_url=None,
    )
    app.state.admin_internal_url = admin_internal_url or os.environ.get(
        "ADC_ADMIN_INTERNAL_URL", DEFAULT_ADMIN_INTERNAL_URL
    )
    app.state.introspector = introspector
    app.state.auth_transport = auth_transport
    app.state.task_store = store
    app.include_router(llm_router)
    app.include_router(a2a_router)

    @app.get("/.well-known/agent-card.json")
    def agent_card() -> dict[str, object]:
        return build_agent_card().model_dump()

    @app.get("/healthz")
    def healthz() -> dict[str, str]:
        return {"status": "ok"}

    @app.get("/readyz")
    async def readyz() -> dict[str, str]:
        if not await store.ready():
            raise HTTPException(status_code=503, detail="A2A task store unavailable")
        return {"status": "ok"}

    @app.get("/v2/agents/evals/suites")
    def list_suites(
        _principal: Annotated[ApplicationPrincipal, Depends(require_evals_read)],
    ) -> dict[str, list[str]]:
        return {"suites": harness.list_suites()}

    @app.get("/v2/agents/evals/suites/{name}", response_model=EvalRunSpec)
    def get_suite(
        name: str,
        _principal: Annotated[ApplicationPrincipal, Depends(require_evals_read)],
    ) -> EvalRunSpec:
        try:
            return harness.load_suite(name)
        except FileNotFoundError as exc:
            raise HTTPException(status_code=404, detail=str(exc)) from exc

    @app.post("/v2/agents/evals/runs", response_model=EvalReport, status_code=201)
    def create_run(
        spec: EvalRunSpec,
        principal: Annotated[ApplicationPrincipal, Depends(require_evals_write)],
    ) -> EvalReport:
        authenticated_spec = spec.model_copy(update={"tenant": principal.tenant_id})
        return harness.run(authenticated_spec, principal.tenant_id, principal.application_id)

    @app.get("/v2/agents/evals/runs/{run_id}", response_model=EvalReport)
    def get_run(
        run_id: str,
        principal: Annotated[ApplicationPrincipal, Depends(require_evals_read)],
    ) -> EvalReport:
        report = harness.report(run_id, principal.tenant_id, principal.application_id)
        if report is None:
            raise HTTPException(status_code=404, detail=f"run not found: {run_id}")
        return report

    return app


app = create_app()


if __name__ == "__main__":
    import uvicorn

    uvicorn.run("app.main:app", host="0.0.0.0", port=8000)
