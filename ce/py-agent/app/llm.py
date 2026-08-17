"""LLM gateway (FR-012, design/82 B4): lightweight OpenAI-compatible proxy.

Deliberately hand-rolled instead of embedding litellm: V1 needs a small,
auditable surface (primary/fallback routing, token metering, PII scrub)
and offline-friendly deployment; litellm integration stays an option for
V1.5 provider breadth.
"""

from __future__ import annotations

import asyncio
import logging
import uuid
from dataclasses import dataclass

import httpx
import redis

logger = logging.getLogger("adc.llm")

USAGE_QUEUE = "adc:usage:queue"


@dataclass
class Usage:
    input_tokens: int
    output_tokens: int

    @property
    def total(self) -> int:
        return self.input_tokens + self.output_tokens


class ProviderError(Exception):
    """A provider call failed; the router may fall back."""


class Provider:
    """OpenAI-compatible chat provider."""

    def __init__(
        self, name: str, base_url: str, api_key: str, model: str, timeout: float = 20.0
    ) -> None:
        self.name = name
        self.model = model
        self._client = httpx.AsyncClient(
            base_url=base_url.rstrip("/"),
            headers={"Authorization": f"Bearer {api_key}"},
            timeout=timeout,
        )

    async def complete(self, messages: list[dict]) -> tuple[str, Usage]:
        try:
            resp = await self._client.post(
                "/chat/completions",
                json={"model": self.model, "messages": messages, "stream": False},
            )
        except httpx.HTTPError as e:
            raise ProviderError(f"{self.name}: transport error: {e}") from e
        if resp.status_code >= 400:
            raise ProviderError(f"{self.name}: upstream {resp.status_code}: {resp.text[:200]}")
        data = resp.json()
        text = data["choices"][0]["message"]["content"]
        u = data.get("usage", {})
        return text, Usage(int(u.get("prompt_tokens", 0)), int(u.get("completion_tokens", 0)))

    async def aclose(self) -> None:
        await self._client.aclose()


class LLMRouter:
    """Primary/fallback routing with 5s failover (FR-012 acceptance)."""

    def __init__(self, primary: Provider, fallback: Provider | None) -> None:
        self.primary = primary
        self.fallback = fallback

    async def complete(self, messages: list[dict]) -> tuple[str, Usage, str]:
        try:
            text, usage = await self.primary.complete(messages)
            return text, usage, self.primary.name
        except ProviderError as e:
            if self.fallback is None:
                raise
            logger.warning("primary provider failed (%s); falling back", e)
            try:
                async with asyncio.timeout(5.0):
                    text, usage = await self.fallback.complete(messages)
            except (ProviderError, TimeoutError) as e2:
                raise ProviderError(f"fallback also failed: {e2}") from e2
            return text, usage, self.fallback.name


def scrub_pii(text: str) -> str:
    """Best-effort PII masking for outbound prompts (FR-012 异常场景)."""
    import re

    text = re.sub(r"\b1[3-9]\d{9}\b", "138****0000", text)  # CN mobile
    text = re.sub(r"\b\d{17}[\dXx]\b", "110101********0000", text)  # CN id
    text = re.sub(r"\b\d{16,19}\b", "****", text)  # bank card
    return text


class TokenMeter:
    """Pushes TOKEN_USAGE events onto the shared Valkey usage queue.

    The payload mirrors the Go metering UsageEvent JSON contract so the
    existing Go worker persists it into adc_usage_events verbatim.
    """

    def __init__(self, valkey_url: str, username: str | None, password: str | None) -> None:
        self._client = redis.Redis.from_url(
            valkey_url or "redis://127.0.0.1:6389/0",
            username=username or None,
            password=password or None,
            decode_responses=False,
        )

    def record(self, tenant_id: str, source: str, tokens: int) -> None:
        event = {
            "tenant_id": tenant_id,
            "kind": "TOKEN_USAGE",
            "source": source,
            "value": tokens,
            "unit": "token",
            "occurred_at": _now_rfc3339(),
            "idempotency_key": str(uuid.uuid4()),
        }
        try:
            self._client.rpush(USAGE_QUEUE, _json_dumps(event))
        except redis.RedisError as e:  # metering must never break the call path
            logger.warning("token meter push failed: %s", e)


def _now_rfc3339() -> str:
    """RFC3339 UTC timestamp; the Go metering worker parses time.Time."""
    import datetime

    return datetime.datetime.now(datetime.UTC).isoformat().replace("+00:00", "Z")


def _json_dumps(obj: dict) -> bytes:
    import json

    return json.dumps(obj).encode()
