"""Home monitor pack tools: "when did you last see the dog?", "what's been going on?"."""

from __future__ import annotations

from collections import Counter
from typing import Any

from argus.protocol import SceneEvent
from argus.scenarios.base import ScenarioTool, ToolContext

MINUTE_MS = 60_000
MAX_COUNTED = 200
LABELS = ("person", "dog", "cat", "package")
DERIVED = frozenset({"state.snapshot"})


def _window(ctx: ToolContext, minutes: Any) -> tuple[int, int]:
    m = float(minutes)
    if not (0 < m <= 60):
        raise ValueError("minutes must be in (0, 60]")
    now = ctx.now_ts_ms()
    return max(0, now - int(m * MINUTE_MS)), now + 1


def _clock(ts_ms: int) -> str:
    return f"{ts_ms // MINUTE_MS:d}:{ts_ms // 1000 % 60:02d}"


def _mentions(e: SceneEvent, label: str) -> bool:
    return e.type.split(".", 1)[0] == label or any(a.label == label for a in e.actors)


async def last_seen(ctx: ToolContext, label: str = "dog", minutes: float = 60, **_: Any) -> str:
    if label not in LABELS:
        raise ValueError(f"label must be one of {', '.join(LABELS)}")
    since, until = _window(ctx, minutes)
    events, _summaries = await ctx.search(label, since_ms=since, until_ms=until)
    hits = [e for e in events if _mentions(e, label)]
    if not hits:
        return f"No {label} seen in the last {minutes:g} minutes."
    e = hits[0]
    return f"The {label} was last seen on {e.stream_id} at {_clock(e.ts_ms)}: {e.summary}"


async def activity(ctx: ToolContext, minutes: float = 15, stream_id: str | None = None, **_: Any) -> str:
    if stream_id is not None and stream_id not in ctx.stream_ids:
        raise ValueError(f"unknown stream_id {stream_id!r}")
    since, until = _window(ctx, minutes)
    events, summaries = await ctx.search(
        None, since_ms=since, until_ms=until, stream_id=stream_id, limit=MAX_COUNTED
    )
    counts = Counter(e.type for e in events if e.type not in DERIVED)
    if not counts:
        return f"Nothing happened in the last {minutes:g} minutes."
    parts = [f"{t} x{n}" for t, n in sorted(counts.items(), key=lambda kv: (-kv[1], kv[0]))]
    out = f"In the last {minutes:g} minutes: " + ", ".join(parts) + "."
    if len(events) >= MAX_COUNTED:
        out += f" (counted the latest {MAX_COUNTED} events only)"
    if summaries:
        out += f" Latest summary: {summaries[-1].text}"
    return out


_MINUTES = {"type": "number", "minimum": 0.1, "maximum": 60}

TOOLS = [
    ScenarioTool(
        name="last_seen",
        description="When and where a person/dog/cat/package was last seen, with what it was doing.",
        input_schema={
            "type": "object",
            "properties": {"label": {"type": "string", "enum": list(LABELS)}, "minutes": _MINUTES},
            "required": ["label"],
            "additionalProperties": False,
        },
        handler=last_seen,
    ),
    ScenarioTool(
        name="activity",
        description="Counts of each kind of event in the last N minutes (default 15), optionally per stream.",
        input_schema={
            "type": "object",
            "properties": {"minutes": _MINUTES, "stream_id": {"type": "string"}},
            "additionalProperties": False,
        },
        handler=activity,
    ),
]
