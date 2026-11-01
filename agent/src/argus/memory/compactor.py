"""Minute compaction: writes one ChatModel-generated summary per (stream, closed minute).

High-severity events are quoted verbatim in the summary text.
"""

from __future__ import annotations

import time
from collections.abc import Callable

from argus.memory.store import MINUTE_MS, MemoryStore
from argus.perception.sanitize import sanitize_summary
from argus.protocol import MinuteSummary, SceneEvent
from argus.providers.base import ChatMessage, ChatModel, ChatOptions, TextDelta
from argus.scenarios.registry import Pack

MAX_SUMMARY_CHARS = 2000


class Compactor:
    def __init__(
        self,
        store: MemoryStore,
        chat: ChatModel,
        pack: Pack,
        *,
        wall_fn: Callable[[], int] | None = None,
    ) -> None:
        self.store = store
        self.chat = chat
        self.pack = pack
        self.wall_fn = wall_fn or (lambda: int(time.time() * 1000))

    def high_severity(self, e: SceneEvent) -> bool:
        return self.pack.severity.get(e.type) == "high"

    async def _summarize(self, events: list[SceneEvent]) -> str:
        body = "\n".join(f"- [{e.type}] {sanitize_summary(e.summary)}" for e in events)
        msgs = [
            ChatMessage(role="system", content=self.pack.minute_summary_prompt),
            ChatMessage(role="user", content=body),
        ]
        out: list[str] = []
        async for ev in self.chat.complete(msgs, options=ChatOptions(max_tokens=200)):
            if isinstance(ev, TextDelta):
                out.append(ev.text)
        return "".join(out).strip()

    async def compact_stream(
        self, stream_id: str, now_ts_ms: int, *, include_open_minute: bool = False
    ) -> list[MinuteSummary]:
        """Summarize every minute of ``stream_id`` that closed before ``now_ts_ms`` and has no summary."""
        cutoff = now_ts_ms + 1 if include_open_minute else now_ts_ms - now_ts_ms % MINUTE_MS
        events = self.store.events_by_ts(stream_id, 0, max(cutoff, 0))
        by_minute: dict[int, list[SceneEvent]] = {}
        for e in events:
            if e.type == "state.snapshot":  # derived; not narrative
                continue
            by_minute.setdefault(e.ts_ms - e.ts_ms % MINUTE_MS, []).append(e)
        made: list[MinuteSummary] = []
        for m in sorted(by_minute):
            if self.store.has_summary(stream_id, m):
                continue
            evs = by_minute[m]
            text = await self._summarize(evs)
            keep = [
                f"{e.type}@{e.ts_ms // 1000}s: {sanitize_summary(e.summary)}"
                for e in evs
                if self.high_severity(e)
            ]
            if keep:
                text = f"{text} Key events: " + " | ".join(keep)
            s = MinuteSummary(
                stream_id=stream_id,
                minute_start_ms=m,
                wall_ms=max(e.wall_ms for e in evs),
                text=text[:MAX_SUMMARY_CHARS],
                event_count=len(evs),
            )
            if self.store.add_summary(s):
                made.append(s)
        return made
