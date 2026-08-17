"""Tests for the LLM gateway core (B4)."""

from __future__ import annotations

import pytest

from app.llm import LLMRouter, Provider, ProviderError, Usage, scrub_pii


class FakeProvider(Provider):
    def __init__(self, name, result=None, error=None):
        self.name = name
        self.model = "m"
        self._result = result
        self._error = error

    async def complete(self, messages):
        if self._error:
            raise self._error
        return self._result


@pytest.mark.asyncio
async def test_primary_success():
    r = LLMRouter(FakeProvider("p", ("ok", Usage(10, 5))), None)
    text, usage, name = await r.complete([{"role": "user", "content": "hi"}])
    assert (text, usage.total, name) == ("ok", 15, "p")


@pytest.mark.asyncio
async def test_fallback_on_primary_error():
    r = LLMRouter(
        FakeProvider("p", error=ProviderError("boom")),
        FakeProvider("f", ("fallback", Usage(1, 1))),
    )
    text, usage, name = await r.complete([{"role": "user", "content": "hi"}])
    assert (text, usage.total, name) == ("fallback", 2, "f")


@pytest.mark.asyncio
async def test_no_fallback_raises():
    r = LLMRouter(FakeProvider("p", error=ProviderError("boom")), None)
    with pytest.raises(ProviderError):
        await r.complete([{"role": "user", "content": "hi"}])


def test_scrub_pii():
    assert scrub_pii("call 13812345678") == "call 138****0000"
    assert "13812345678" not in scrub_pii("id 110101199001011234 please")
    assert scrub_pii("card 6222021234567890123") == "card ****"
