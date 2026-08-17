"""FastAPI routes for the LLM gateway (FR-012 / design/82 B4).

Mounted under /v2/agents/llm/* behind the unified gateway.
"""

from __future__ import annotations

import os
from typing import Any

from fastapi import APIRouter, HTTPException
from pydantic import BaseModel, Field

from app.llm import LLMRouter, Provider, ProviderError, TokenMeter, scrub_pii

router = APIRouter(prefix="/v2/agents/llm", tags=["llm"])


class ChatRequest(BaseModel):
    model: str | None = None
    messages: list[dict[str, Any]]
    tenant_id: str | None = Field(
        default=None, description="billing scope (authenticated upstream)"
    )
    stream: bool = False


class ChatResponse(BaseModel):
    provider: str
    content: str
    usage: dict[str, int]


def _providers_from_env() -> LLMRouter:
    primary = Provider(
        "primary",
        os.environ.get("LLM_PRIMARY_BASE", "https://api.openai.com/v1"),
        os.environ.get("LLM_PRIMARY_KEY", ""),
        os.environ.get("LLM_PRIMARY_MODEL", "gpt-4o-mini"),
    )
    fallback: Provider | None = None
    if os.environ.get("LLM_SECONDARY_KEY"):
        fallback = Provider(
            "fallback",
            os.environ.get("LLM_SECONDARY_BASE", "https://api.openai.com/v1"),
            os.environ.get("LLM_SECONDARY_KEY", ""),
            os.environ.get("LLM_SECONDARY_MODEL", "gpt-4o-mini"),
        )
    return LLMRouter(primary, fallback)


_router = _providers_from_env()
_meter = TokenMeter(
    os.environ.get("VALKEY_URL", "redis://127.0.0.1:6389/0"),
    os.environ.get("VALKEY_USERNAME"),
    os.environ.get("VALKEY_PASSWORD"),
)


@router.get("/models")
def models() -> dict[str, Any]:
    return {
        "providers": [
            {"name": _router.primary.name, "model": _router.primary.model},
            *(
                [{"name": _router.fallback.name, "model": _router.fallback.model}]
                if _router.fallback
                else []
            ),
        ]
    }


@router.post("/chat/completions", response_model=ChatResponse)
async def chat(req: ChatRequest) -> ChatResponse:
    if req.stream:
        raise HTTPException(status_code=400, detail="stream=true is not supported in V1")
    if not req.messages:
        raise HTTPException(status_code=400, detail="messages must not be empty")
    messages = [{**m, "content": scrub_pii(str(m.get("content", "")))} for m in req.messages]
    try:
        text, usage, provider = await _router.complete(messages)
    except ProviderError as e:
        raise HTTPException(status_code=502, detail=str(e)) from e
    if req.tenant_id:
        _meter.record(req.tenant_id, provider, usage.total)
    return ChatResponse(
        provider=provider,
        content=text,
        usage={
            "input_tokens": usage.input_tokens,
            "output_tokens": usage.output_tokens,
            "total_tokens": usage.total,
        },
    )


@router.get("/usage")
def usage(tenant_id: str) -> dict[str, Any]:
    # Aggregated usage lives in adc_usage_events (Go billing engine, B3);
    # this endpoint is a thin forwarder placeholder until the billing API
    # is mounted here or proxied.
    return {"tenant_id": tenant_id, "aggregated": "via /v1/admin/billing/statements"}
