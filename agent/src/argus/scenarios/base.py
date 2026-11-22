"""Base types for scenario packs."""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from typing import TYPE_CHECKING, Any

from argus.protocol import MinuteSummary, SceneEvent

if TYPE_CHECKING:
    from argus.memory.store import MemoryStore

CORE_TOOL_NAMES = frozenset({"search_history", "what_happened", "describe_now"})


class ScenarioState:
    """Pure, deterministic reducer over accepted events (applied in ``ts_ms`` order)."""

    def apply(self, event: SceneEvent) -> None:  # pragma: no cover - interface
        raise NotImplementedError

    def snapshot(self) -> dict[str, Any]:  # pragma: no cover - interface
        raise NotImplementedError


class NullState(ScenarioState):
    def apply(self, event: SceneEvent) -> None:
        return None

    def snapshot(self) -> dict[str, Any]:
        return {}


@dataclass(slots=True)
class ToolContext:
    """What a pack tool handler can touch."""

    session_id: str
    scenario: str
    stream_ids: list[str]
    state: ScenarioState
    memory: MemoryStore | None
    now_ts_ms: Callable[[], int]

    def snapshot(self) -> dict[str, Any]:
        return self.state.snapshot()

    async def search(
        self, text: str | None, *, since_ms: int, until_ms: int, stream_id: str | None = None, limit: int = 20
    ) -> tuple[list[SceneEvent], list[MinuteSummary]]:
        if self.memory is None:
            return [], []
        r = await self.memory.search(stream_id, since_ms=since_ms, until_ms=until_ms, text=text, limit=limit)
        return r.events, r.summaries


ToolHandler = Callable[..., Awaitable[str | dict[str, Any]]]


@dataclass(slots=True)
class ScenarioTool:
    name: str
    description: str
    input_schema: dict[str, Any]
    handler: ToolHandler  # async (ctx: ToolContext, **args) -> str | dict


@dataclass(slots=True)
class EventTypeSpec:
    type: str
    description: str
    attrs: dict[str, str] = field(default_factory=dict)


@dataclass(slots=True)
class Sampling:
    min_motion: float = 0.02
    max_interval_ms: int = 5000
    batch_frames: int = 3
    event_cooldown_s: float = 30.0  # collapse same-type events within this window (0 disables)
    cooldowns: dict[str, float] = field(default_factory=dict)  # per-type overrides
    max_events_per_minute: int = 60  # per-stream cap; lowest-confidence dropped first
