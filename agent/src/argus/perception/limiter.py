"""Per-stream scene-event rate limiting: cooldown collapsing, per-minute cap, exemptions.

Time is the session clock (``ts_ms``, monotonic); ``wall_ms`` is only recorded for display.
"""

from __future__ import annotations

from dataclasses import dataclass
from typing import Literal

from argus.metrics import Metrics
from argus.protocol import SceneEvent
from argus.scenarios.registry import Pack

SNAPSHOT_TYPE = "state.snapshot"
REPEAT_THRESHOLDS = frozenset({10, 100, 1000})
WINDOW_MS = 60_000


@dataclass(slots=True)
class Decision:
    event: SceneEvent
    action: Literal["emit", "collapse", "drop"]
    target: SceneEvent | None = None  # collapse: the first event that absorbed this one
    crossed: bool = False  # collapse: target's repeat_count just reached 10/100/1000


@dataclass(slots=True)
class _Track:
    event: SceneEvent
    last_ts: int
    count: int = 1


class EventLimiter:
    def __init__(self, pack: Pack, stream_id: str, metrics: Metrics | None = None) -> None:
        s = pack.sampling
        self.stream_id = stream_id
        self.metrics = metrics or Metrics()
        self.default_cooldown_ms = int(s.event_cooldown_s * 1000)
        self.cooldown_ms = {t: int(v * 1000) for t, v in s.cooldowns.items()}
        self.cap = s.max_events_per_minute
        self.high = {t for t, sev in pack.severity.items() if sev == "high"}
        self._tracks: dict[str, _Track] = {}
        self._window: list[int] = []  # ts_ms of events admitted in the last minute

    def exempt(self, etype: str) -> bool:
        return etype == SNAPSHOT_TYPE or etype in self.high

    def process(self, events: list[SceneEvent]) -> list[Decision]:
        """Classify a ts-ordered batch. Collapsing mutates the first event's attrs in place."""
        out: list[Decision] = []
        cands: list[Decision] = []
        for ev in events:
            if self.exempt(ev.type):
                d = Decision(ev, "emit")
                self._window.append(ev.ts_ms)
            elif (tr := self._tracks.get(ev.type)) and ev.ts_ms - tr.last_ts < self.cooldown_ms.get(
                ev.type, self.default_cooldown_ms
            ):
                d = self._collapse(tr, ev)
            else:
                d = Decision(ev, "emit")
                self._tracks[ev.type] = _Track(ev, ev.ts_ms)
                cands.append(d)
            out.append(d)
        if events:
            horizon = max(e.ts_ms for e in events) - WINDOW_MS
            self._window = [t for t in self._window if t > horizon]
        budget = max(0, self.cap - len(self._window))
        keep = sorted(cands, key=lambda d: -d.event.confidence)[:budget]  # stable: ties keep ts order
        for d in cands:
            if d in keep:
                self._window.append(d.event.ts_ms)
                continue
            d.action = "drop"
            self.metrics.inc("argus_events_rate_limited_total", stream=self.stream_id, reason="cap")
            if self._tracks.get(d.event.type) and self._tracks[d.event.type].event is d.event:
                del self._tracks[d.event.type]
        for d in out:  # followers of a dropped event are dropped with it
            if (
                d.action == "collapse"
                and d.target is not None
                and any(c.event is d.target and c.action == "drop" for c in cands)
            ):
                d.action, d.crossed = "drop", False
        return out

    def _collapse(self, tr: _Track, ev: SceneEvent) -> Decision:
        tr.count += 1
        tr.last_ts = max(tr.last_ts, ev.ts_ms)
        tr.event.attrs["repeat_count"] = tr.count
        tr.event.attrs["last_seen_wall_ms"] = ev.wall_ms
        self.metrics.inc("argus_events_collapsed_total", stream=self.stream_id, type=ev.type)
        return Decision(ev, "collapse", tr.event, tr.count in REPEAT_THRESHOLDS)
