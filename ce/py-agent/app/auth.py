"""Real-time developer application authentication for agent-plane routes."""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from typing import Literal

import httpx
from fastapi import HTTPException, Request
from pydantic import BaseModel, Field

DEFAULT_ADMIN_INTERNAL_URL = "http://adc:8080"
SCOPE_TASKS_READ = "a2a.tasks:read"
SCOPE_TASKS_WRITE = "a2a.tasks:write"
SCOPE_LLM_INVOKE = "llm:invoke"
SCOPE_EVALS_READ = "evals:read"
SCOPE_EVALS_WRITE = "evals:write"


class ApplicationPrincipal(BaseModel):
    active: Literal[True]
    application_id: str = Field(min_length=1)
    tenant_id: str = Field(min_length=1)
    required_scope: str


Introspector = Callable[[Request, str], Awaitable[ApplicationPrincipal]]


async def introspect_application(request: Request, required_scope: str) -> ApplicationPrincipal:
    headers = {
        name: value
        for name in ("Authorization", "X-ADC-Application-Credential")
        if (value := request.headers.get(name))
    }
    url = request.app.state.admin_internal_url.rstrip("/") + "/v1/developer/introspection"
    try:
        async with httpx.AsyncClient(
            timeout=3.0, transport=request.app.state.auth_transport
        ) as client:
            response = await client.post(
                url, json={"required_scope": required_scope}, headers=headers
            )
    except httpx.HTTPError as exc:
        raise HTTPException(status_code=503, detail="credential verification unavailable") from exc
    if response.status_code in (401, 403):
        raise HTTPException(
            status_code=response.status_code, detail="application credential rejected"
        )
    if response.status_code != 200:
        raise HTTPException(status_code=503, detail="credential verification unavailable")
    try:
        principal = ApplicationPrincipal.model_validate(response.json())
    except (ValueError, TypeError) as exc:
        raise HTTPException(
            status_code=503, detail="invalid credential verification response"
        ) from exc
    if principal.required_scope != required_scope:
        raise HTTPException(status_code=503, detail="invalid credential verification response")
    return principal


async def require_scope(request: Request, scope: str) -> ApplicationPrincipal:
    introspector: Introspector = request.app.state.introspector
    return await introspector(request, scope)


async def require_tasks_read(request: Request) -> ApplicationPrincipal:
    return await require_scope(request, SCOPE_TASKS_READ)


async def require_tasks_write(request: Request) -> ApplicationPrincipal:
    return await require_scope(request, SCOPE_TASKS_WRITE)


async def require_llm_invoke(request: Request) -> ApplicationPrincipal:
    return await require_scope(request, SCOPE_LLM_INVOKE)


async def require_evals_read(request: Request) -> ApplicationPrincipal:
    return await require_scope(request, SCOPE_EVALS_READ)


async def require_evals_write(request: Request) -> ApplicationPrincipal:
    return await require_scope(request, SCOPE_EVALS_WRITE)
