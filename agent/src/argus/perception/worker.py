"""Perception worker: one asyncio task group per (session, stream).

frames WS (gateway) -> bounded drop-oldest buffer -> batches of ``sampling.batch_frames`` ->
``VisionModel.observe`` -> validate/filter per PACK_FORMAT -> sanitize -> memory + session + State.
"""

from __future__ import annotations

import asyncio
import logging
import struct
import uuid
from collections import deque
from collections.abc import AsyncIterator, Awaitable, Callable
from typing import Any

import websockets

from argus.clock import SessionClock
from argus.memory.store import MemoryStore
from argus.metrics import Metrics
from argus.perception.limiter import SNAPSHOT_TYPE, EventLimiter
from argus.perception.sanitize import Sanitizer
from argus.protocol import Actor, BBox, FrameRef, SceneEvent
from argus.providers.base import Frame, FrameMeta, Observation, ProviderError, VisionModel
from argus.scenarios.base import ScenarioState
from argus.scenarios.registry import Pack

log = logging.getLogger(__name__)

HEADER = struct.Struct("<Iqi")  # uint32 stream_idx, int64 ts_ms, int32 jpeg_len (16 bytes)
MAX_BINARY = 256 * 1024
MAX_SUMMARY = 500
FrameSource = AsyncIterator[Frame]


def jpeg_size(data: bytes) -> tuple[int, int]:
    """Best-effort (width, height) from a JPEG SOF marker; (0, 0) if unknown."""
    i = 2
    while i + 9 < len(data):
        if data[i] != 0xFF:
            i += 1
            continue
        m = data[i + 1]
        if m in (0xC0, 0xC1, 0xC2):
            h, w = struct.unpack(">HH", data[i + 5 : i + 9])
            return w, h
        if m in (0xD8, 0x01) or 0xD0 <= m <= 0xD7:
            i += 2
            continue
        i += 2 + struct.unpack(">H", data[i + 2 : i + 4])[0]
    return 0, 0


def parse_frame_message(msg: bytes, stream_id: str, clock: SessionClock) -> Frame:
    """Parse ``16-byte LE header + JPEG``. Raises ValueError on malformed input."""
    if len(msg) < HEADER.size or len(msg) > MAX_BINARY + HEADER.size:
        raise ValueError(f"bad frame message size {len(msg)}")
    _idx, ts_ms, n = HEADER.unpack_from(msg)
    if n < 0 or n != len(msg) - HEADER.size or ts_ms < 0:
        raise ValueError("frame header/length mismatch")
    jpeg = bytes(msg[HEADER.size :])
    clock.observe(ts_ms)
    w, h = jpeg_size(jpeg)
    return Frame(jpeg, FrameMeta(stream_id, ts_ms, clock.wall_of(ts_ms), w, h))


async def gateway_frames(
    base_url: str, stream_id: str, pack: Pack, clock: SessionClock, *, token: str | None
) -> FrameSource:
    """Yield frames from ``{base_url}/internal/frames/{stream_id}``; raises on disconnect."""
    q = f"?min_motion={pack.sampling.min_motion}&max_interval_ms={pack.sampling.max_interval_ms}"
    headers = {"X-Internal-Token": token} if token else {}
    async with websockets.connect(
        f"{base_url.rstrip('/')}/internal/frames/{stream_id}{q}",
        additional_headers=headers,
        max_size=MAX_BINARY + HEADER.size,
    ) as ws:
        async for msg in ws:
            if isinstance(msg, bytes):
                try:
                    yield parse_frame_message(msg, stream_id, clock)
                except ValueError as e:
                    log.warning("dropping malformed frame: %s", e)


class PerceptionWorker:
    def __init__(
        self,
        *,
        session_id: str,
        stream_id: str,
        pack: Pack,
        vision: VisionModel,
        store: MemoryStore | None,
        clock: SessionClock,
        on_event: Callable[[SceneEvent], Awaitable[None]],
        state: ScenarioState | None = None,
        metrics: Metrics | None = None,
        sanitizer: Sanitizer | None = None,
        limiter: EventLimiter | None = None,
        on_frame: Callable[[Frame], None] | None = None,
        buffer_batches: int = 4,
        flush_after_s: float | None = None,
    ) -> None:
        self.session_id = session_id
        self.stream_id = stream_id
        self.pack = pack
        self.vision = vision
        self.store = store
        self.clock = clock
        self.on_event = on_event
        self.state = state
        self.metrics = metrics or Metrics()
        self.sanitizer = sanitizer or Sanitizer()
        self.limiter = limiter or EventLimiter(pack, stream_id, self.metrics)
        self.on_frame = on_frame
        self.batch = pack.sampling.batch_frames
        self._buf: deque[Frame] = deque(maxlen=self.batch * buffer_batches)
        self._wake = asyncio.Event()
        self._closed = False
        self.flush_after_s = (
            flush_after_s if flush_after_s is not None else pack.sampling.max_interval_ms * 2 / 1000
        )
        self.events_emitted = 0
        self._idle = asyncio.Event()
        self._idle.set()

    # ------------------------------------------------------------ ingest
    def push(self, frame: Frame) -> None:
        """Enqueue a frame; drops the oldest when the buffer is full (never queues unbounded)."""
        if len(self._buf) == self._buf.maxlen:
            self.metrics.inc("argus_frames_dropped_total", stream=self.stream_id)
        self._buf.append(frame)
        self.metrics.inc("argus_frames_received_total", stream=self.stream_id)
        if self.on_frame:
            self.on_frame(frame)
        self._idle.clear()
        self._wake.set()

    async def consume(self, source: FrameSource) -> None:
        async for f in source:
            self.push(f)

    async def run(self, source: FrameSource) -> None:
        """Run reader + processor until the source ends (then flush) or cancellation."""
        proc = asyncio.create_task(self._process_loop())
        try:
            await self.consume(source)
        finally:
            self._closed = True
            self._wake.set()
            try:
                await proc
            except asyncio.CancelledError:
                proc.cancel()
                raise

    async def drain(self) -> None:
        """Wait until every buffered frame has been processed (used by demo/tests)."""
        await self._idle.wait()

    # --------------------------------------------------------- processing
    async def _process_loop(self) -> None:
        while True:
            if not self._buf:
                self._idle.set()
                if self._closed:
                    return
                self._wake.clear()
                await self._wake.wait()
                continue
            if len(self._buf) < self.batch and not self._closed:
                self._wake.clear()
                try:
                    await asyncio.wait_for(self._wake.wait(), timeout=self.flush_after_s)
                except TimeoutError:
                    pass  # flush the partial batch below
                else:
                    continue  # more frames arrived; re-evaluate
            frames = [self._buf.popleft() for _ in range(min(self.batch, len(self._buf)))]
            try:
                await self.process_batch(frames)
            except asyncio.CancelledError:
                raise
            except Exception:
                log.exception("perception batch failed", extra={"x_stream": self.stream_id})
                self.metrics.inc("argus_perception_errors_total", stream=self.stream_id)

    def _context(self) -> str:
        if self.store is None:
            return ""
        return "\n".join(
            f"[{e.ts_ms // 1000}s] {e.type}: {self.sanitizer.sanitize(e.summary)}"
            for e in self.store.recent_events(self.stream_id, 5)
        )

    async def process_batch(self, frames: list[Frame]) -> list[SceneEvent]:
        try:
            obs = await self.vision.observe(
                frames,
                system_prompt=self.pack.vision_system,
                event_types=list(self.pack.event_types),
                context=self._context(),
            )
        except ProviderError as e:
            log.warning("vision provider error: %s", e)
            self.metrics.inc("argus_vision_errors_total", stream=self.stream_id)
            return []
        events = [ev for o in obs if (ev := self.build_event(o, frames)) is not None]
        events.sort(key=lambda e: e.ts_ms)  # stable: fixture/model order preserved within a ts
        decisions = self.limiter.process(events)
        fresh = [d.event for d in decisions if d.action == "emit"]
        if fresh and self.store is not None:
            self.store.add_events(fresh)
        out: list[SceneEvent] = []
        for d in decisions:
            if d.action == "collapse" and d.target is not None:
                if self.store is not None:
                    self.store.update_attrs(d.target.id, d.target.attrs)
                if d.crossed:  # same id, updated attrs: consumers replace the earlier event
                    self.metrics.inc("argus_event_updates_total", stream=self.stream_id)
                    await self.on_event(d.target.model_copy(deep=True))
            elif d.action == "emit":
                ev = d.event
                out.append(ev)
                await self._emit(ev)
                snap = self._apply_state(ev)
                if snap is not None:
                    out.append(snap)
                    await self._emit(snap)
        return out

    async def _emit(self, ev: SceneEvent) -> None:
        await self.on_event(ev)
        self.events_emitted += 1
        self.metrics.inc("argus_events_total", type=ev.type)

    def _apply_state(self, ev: SceneEvent) -> SceneEvent | None:
        """Feed the pack State; if it reports a scoring change, return a derived ``state.snapshot`` event."""
        st = self.state
        if st is None:
            return None
        before = getattr(st, "scoring_version", None)
        st.apply(ev)
        if before is None or getattr(st, "scoring_version", before) == before:
            return None
        if SNAPSHOT_TYPE not in self.pack.event_types:
            return None
        snap = st.snapshot()
        sev = SceneEvent(
            id=str(uuid.uuid4()), stream_id=ev.stream_id, ts_ms=ev.ts_ms, wall_ms=ev.wall_ms,
            type=SNAPSHOT_TYPE, confidence=1.0, attrs=snap, summary=f"State snapshot after {ev.type}",
            frame_ref=ev.frame_ref, scenario=ev.scenario,
        )  # fmt: skip
        if self.store is not None:
            self.store.add_events([sev])
        return sev

    def _drop(self, reason: str) -> None:
        self.metrics.inc("argus_observations_dropped_total", reason=reason)

    def build_event(self, o: Observation, frames: list[Frame]) -> SceneEvent | None:
        """Validate/filter one observation per PACK_FORMAT; None if it must be dropped."""
        if o.event_type == "none":
            return None
        spec = self.pack.event_types.get(o.event_type)
        if o.event_type == SNAPSHOT_TYPE:
            self._drop("derived_type")
            return None
        if spec is None:
            self._drop("unknown_type")
            return None
        if not (0 <= o.frame_index < len(frames)):
            self._drop("bad_frame_index")
            return None
        try:
            conf = float(o.confidence)
        except (TypeError, ValueError):
            conf = float("nan")
        if conf != conf:
            self._drop("bad_confidence")
            return None
        conf = min(1.0, max(0.0, conf))
        meta = frames[o.frame_index].meta
        attrs: dict[str, Any] = {}
        for k, v in o.attrs.items():
            if k in spec.attrs:
                attrs[k] = self.sanitizer.sanitize(v) if isinstance(v, str) else v
            else:
                self._drop("undeclared_attr")
        summary = self.sanitizer.sanitize(o.summary)[:MAX_SUMMARY] or f"{o.event_type} (summary withheld)"
        return SceneEvent(
            id=str(uuid.uuid4()),
            stream_id=meta.stream_id,
            ts_ms=meta.ts_ms,
            wall_ms=meta.wall_ms,
            type=o.event_type,
            confidence=conf,
            actors=self._actors(o.actors),
            attrs=attrs,
            summary=summary,
            frame_ref=FrameRef(stream_id=meta.stream_id, ts_ms=meta.ts_ms),
            scenario=self.pack.name,
        )

    def _actors(self, raw: tuple[dict[str, Any], ...]) -> list[Actor]:
        out: list[Actor] = []
        for a in raw:
            label = a.get("label")
            if not isinstance(label, str) or (self.pack.actors and label not in self.pack.actors):
                self._drop("unknown_actor")
                continue
            try:
                bbox = BBox(**a["bbox"]) if isinstance(a.get("bbox"), dict) else None
                out.append(
                    Actor(label=label, track_id=a.get("track_id"), bbox=bbox, confidence=a.get("confidence"))
                )
            except ValueError:
                self._drop("bad_actor")
        return out
