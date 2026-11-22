"""`ollama` provider: ChatModel + Embedder over Ollama's HTTP API (httpx).

Vision, detector, ASR and TTS stay registered as stubs so config errors are explicit.
"""

from __future__ import annotations

import asyncio
import base64
import math
import os
import time
from collections.abc import AsyncIterator, Sequence
from typing import Any

import httpx

from argus.providers.base import (
    ChatEvent,
    ChatMessage,
    ChatOptions,
    Done,
    ProviderError,
    ProviderRateLimited,
    ProviderUnavailable,
    TextDelta,
    ToolCall,
    ToolCallEvent,
    ToolSpec,
    Usage,
)
from argus.providers.registry import KINDS, register

NAME = "ollama"
DEFAULT_HOST = "http://127.0.0.1:11434"


class _OllamaBase:
    """Shared HTTP plumbing: request building, retry with backoff and error mapping."""

    def __init__(
        self,
        model: str,
        *,
        base_url: str | None = None,
        timeout_s: float = 60.0,
        max_retries: int = 2,
        backoff_s: float = 0.25,
        transport: httpx.AsyncBaseTransport | None = None,
    ) -> None:
        """Create the client.

        Args:
            model: Ollama model tag, e.g. ``llama3.2``.
            base_url: Server URL; defaults to ``$OLLAMA_HOST`` or the local daemon.
            timeout_s: Per-request timeout in seconds.
            num_retries: Extra attempts after the first for retryable failures.
            backoff_s: Base delay of the exponential backoff between attempts.
            api_key: Bearer token for a proxied server.
            transport: Optional httpx transport (tests inject a mock).
        """
        self.model = model
        self.timeout_s = timeout_s
        self.max_retries = max_retries
        self.backoff_s = backoff_s
        self.retries_used = 0
        self._base_url = base_url
        self._transport = transport

    def _headers(self) -> dict[str, str]:
        return {"Accept": "application/json"}

    def _retry_delay(self, attempt: int) -> float:
        return self.backoff_s * 2 ** (attempt - 1)

    def _error(self, r: httpx.Response) -> ProviderError:
        msg = f"ollama HTTP {r.status_code}"
        if r.status_code == 429:
            try:
                after = float(r.headers["retry-after"])
            except (KeyError, ValueError):
                after = None
            return ProviderRateLimited(msg, retry_after_s=after, provider=NAME)
        return ProviderError(msg, retryable=r.status_code >= 500, provider=NAME)

    async def _send(self, path: str, body: dict[str, Any]) -> dict[str, Any]:
        base = (self._base_url or os.environ.get("OLLAMA_HOST", DEFAULT_HOST)).rstrip("/")
        started = time.time()
        last: ProviderError | None = None
        for attempt in range(self.max_retries + 1):
            if attempt:
                self.retries_used += 1
                await asyncio.sleep(self._retry_delay(attempt))
            try:
                async with httpx.AsyncClient(transport=self._transport, timeout=self.timeout_s) as client:
                    r = await client.post(base + path, json=body, headers=self._headers())
            except httpx.TransportError as e:
                last = ProviderUnavailable(
                    f"ollama unreachable: {type(e).__name__}", retryable=True, provider=NAME
                )
                continue
            if r.status_code < 400:
                return r.json()
            last = self._error(r)
            if not last.retryable or time.time() - started > self.timeout_s * 2:
                break
        assert last is not None
        raise last


def _wire_message(m: ChatMessage) -> dict[str, Any]:
    out: dict[str, Any] = {"role": m.role, "content": m.content}
    if m.tool_result is not None:
        out["content"] = m.tool_result.content
    if m.images:
        out["images"] = [base64.b64encode(i.jpeg).decode() for i in m.images]
    if m.tool_calls:
        out["tool_calls"] = [{"function": {"name": c.name, "arguments": c.arguments}} for c in m.tool_calls]
    return out


class OllamaChat(_OllamaBase):
    name = "ollama-chat"

    def _retry_delay(self, attempt: int) -> float:
        return super()._retry_delay(attempt) * 4  # a cold model load takes a while

    async def complete(
        self,
        messages: Sequence[ChatMessage],
        *,
        tools: Sequence[ToolSpec] = (),
        options: ChatOptions | None = None,
    ) -> AsyncIterator[ChatEvent]:
        body: dict[str, Any] = {
            "model": self.model,
            "stream": False,
            "messages": [_wire_message(m) for m in messages],
            "options": {"num_predict": ChatOptions().max_tokens if options is None else options.max_tokens},
        }
        if tools:
            body["tools"] = [
                {
                    "type": "function",
                    "function": {"name": t.name, "description": t.description, "parameters": t.input_schema},
                }
                for t in tools
            ]
        data = await self._send("/api/chat", body)
        msg = data.get("message") or {}
        if msg.get("content"):
            yield TextDelta(msg["content"])
        calls = msg.get("tool_calls") or []
        for i, c in enumerate(calls):
            fn = c["function"]
            yield ToolCallEvent(ToolCall(f"call_{i}", fn["name"], dict(fn.get("arguments") or {})))
        reason = (
            "tool_use" if calls else ("max_tokens" if data.get("done_reason") == "length" else "end_turn")
        )
        usage = Usage(data.get("prompt_eval_count", 0), data.get("eval_count", 0))
        yield Done(reason, usage)


class OllamaEmbedder(_OllamaBase):
    name = "ollama-embedder"

    def __init__(self, model: str, *, dim: int, **kw: Any) -> None:
        super().__init__(model, **kw)
        self.dim = dim

    async def embed(self, texts: Sequence[str]) -> list[list[float]]:
        if not texts:
            return []
        data = await self._send("/api/embed", {"model": self.model, "input": list(texts)})
        vecs = data.get("embeddings") or []
        if len(vecs) != len(texts) or any(len(v) != self.dim for v in vecs):
            raise ProviderError(
                f"ollama returned unexpected embedding shape (want dim {self.dim})", provider=NAME
            )
        return [[x / (math.sqrt(sum(y * y for y in v)) or 1.0) for x in v] for v in vecs]


def _args(cfg: dict[str, Any]) -> dict[str, Any]:
    if "model" not in cfg:
        raise ProviderUnavailable("ollama provider needs `model`", provider=NAME)
    keys = ("base_url", "timeout_s", "max_retries", "backoff_s")
    return {"model": cfg["model"], **{k: cfg[k] for k in keys if k in cfg}}


def _chat_factory(cfg: dict[str, Any]) -> Any:
    return OllamaChat(**_args(cfg))


def _embedder_factory(cfg: dict[str, Any]) -> Any:
    if "dim" not in cfg:
        raise ProviderUnavailable("ollama embedder needs `dim`", provider=NAME)
    return OllamaEmbedder(dim=int(cfg["dim"]), **_args(cfg))


def _stub(_cfg: dict[str, Any]) -> Any:
    raise ProviderUnavailable("not implemented", provider=NAME)


for _kind in KINDS:
    register(_kind, NAME, {"chat": _chat_factory, "embedder": _embedder_factory}.get(_kind, _stub))
