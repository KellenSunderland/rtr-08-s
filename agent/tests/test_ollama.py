import json
import math

import httpx
import pytest

from argus.providers import registry
from argus.providers.base import (
    ChatMessage,
    Done,
    ProviderRateLimited,
    ProviderUnavailable,
    TextDelta,
)
from argus.providers.ollama import OllamaChat, OllamaEmbedder


def transport(handler):
    return httpx.MockTransport(handler)


async def collect(agen):
    return [x async for x in agen]


async def test_chat_text_and_usage():
    seen = {}

    def handler(req: httpx.Request) -> httpx.Response:
        seen["url"], seen["body"] = str(req.url), json.loads(req.content)
        return httpx.Response(
            200,
            json={
                "message": {"role": "assistant", "content": "hi there"},
                "prompt_eval_count": 7,
                "eval_count": 3,
            },
        )

    chat = OllamaChat("llama3.2", base_url="http://ollama.test", transport=transport(handler))
    ev = await collect(chat.complete([ChatMessage("system", "be brief"), ChatMessage("user", "hello")]))
    assert seen["url"] == "http://ollama.test/api/chat" and seen["body"]["stream"] is False
    assert [m["role"] for m in seen["body"]["messages"]] == ["system", "user"]
    assert isinstance(ev[0], TextDelta) and ev[0].text == "hi there"
    assert isinstance(ev[-1], Done) and ev[-1].stop_reason == "end_turn" and ev[-1].usage.input_tokens == 7


async def test_retries_5xx_then_succeeds():
    n = 0

    def handler(req: httpx.Request) -> httpx.Response:
        nonlocal n
        n += 1
        return httpx.Response(503) if n < 3 else httpx.Response(200, json={"message": {"content": "ok"}})

    chat = OllamaChat("m", base_url="http://x", backoff_s=0, transport=transport(handler))
    ev = await collect(chat.complete([ChatMessage("user", "x")]))
    assert ev[0].text == "ok" and n == 3 and chat.retries_used == 2


async def test_rate_limit_error():
    chat = OllamaChat(
        "m", base_url="http://x", max_retries=1, backoff_s=0,
        transport=transport(lambda r: httpx.Response(429, headers={"retry-after": "2"})),
    )  # fmt: skip
    with pytest.raises(ProviderRateLimited) as ei:
        await collect(chat.complete([ChatMessage("user", "x")]))
    assert ei.value.retry_after_s == 2.0 and chat.retries_used == 1


async def test_embedder_normalizes_and_checks_dim():
    def handler(req: httpx.Request) -> httpx.Response:
        assert json.loads(req.content)["input"] == ["a", "b"]
        return httpx.Response(200, json={"embeddings": [[3.0, 4.0], [0.0, 2.0]]})

    emb = OllamaEmbedder("nomic", dim=2, base_url="http://x", transport=transport(handler))
    a, b = await emb.embed(["a", "b"])
    assert math.isclose(a[0], 0.6) and math.isclose(b[1], 1.0)


async def test_base_url_from_env_and_connect_error(monkeypatch):
    monkeypatch.setenv("OLLAMA_HOST", "http://env.test:11434/")

    def handler(req: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("refused")

    chat = OllamaChat("m", max_retries=0, transport=transport(handler))
    with pytest.raises(ProviderUnavailable, match="unreachable"):
        await collect(chat.complete([ChatMessage("user", "x")]))


def test_registry_builds_chat_and_embedder_but_keeps_other_stubs():
    assert isinstance(registry.make_provider("chat", {"provider": "ollama", "model": "m"}), OllamaChat)
    emb = registry.make_provider("embedder", {"provider": "ollama", "model": "m", "dim": 8})
    assert emb.dim == 8
    with pytest.raises(ProviderUnavailable, match="needs `dim`"):
        registry.make_provider("embedder", {"provider": "ollama", "model": "m"})
    with pytest.raises(ProviderUnavailable, match="not implemented"):
        registry.make_provider("vision", {"provider": "ollama"})
