"""SQLite + sqlite-vec memory store (schema in ``schema.sql``).

All SQL is parameterised. DB calls are short and run inline on the event loop thread; a lock guards the
single connection. Embeddings are produced lazily in batches by :meth:`MemoryStore.embed_pending`.

Time-window semantics (CONTRACTS.md "History time domain"): ``since_ms <= wall_ms < until_ms``
(half-open) on **wall-clock epoch ms**, for events and minute summaries alike (history spans sessions).
"""

from __future__ import annotations

import json
import sqlite3
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import sqlite_vec

from argus.protocol import Actor, FrameRef, MinuteSummary, SceneEvent
from argus.providers.base import Embedder

SCHEMA_VERSION = "1"
MINUTE_MS = 60_000
W_COS, W_RECENCY = 0.7, 0.3
_MAX_CANDIDATES = 5000


class MemoryDimMismatch(RuntimeError):
    """The DB was created with a different embedder dimension; rebuild required."""


@dataclass(slots=True)
class SearchResult:
    events: list[SceneEvent] = field(default_factory=list)
    summaries: list[MinuteSummary] = field(default_factory=list)
    truncated: bool = False
    scores: dict[str, float] = field(default_factory=dict)  # event id -> rerank score (text queries)


def _blob(v: list[float]) -> bytes:
    return sqlite_vec.serialize_float32(v)


class MemoryStore:
    def __init__(self, path: str | Path, embedder: Embedder, *, retention_s: int = 3600) -> None:
        self.path = str(path)
        self.embedder = embedder
        self.retention_s = retention_s
        self._lock = threading.RLock()
        self.db = sqlite3.connect(self.path, check_same_thread=False, isolation_level=None)
        self.db.row_factory = sqlite3.Row
        self.db.enable_load_extension(True)
        sqlite_vec.load(self.db)
        self.db.enable_load_extension(False)
        self._init()

    # ------------------------------------------------------------ setup
    def _init(self) -> None:
        schema = (Path(__file__).parent / "schema.sql").read_text()
        with self._lock:
            self.db.executescript(schema)
            dim = self.embedder.dim
            row = self._meta("embedder_dim")
            if row is None:
                self._set_meta("schema_version", SCHEMA_VERSION)
                self._set_meta("embedder_name", self.embedder.name)
                self._set_meta("embedder_dim", str(dim))
                self._set_meta("retention_s", str(self.retention_s))
            elif int(row) != dim:
                self.db.close()
                raise MemoryDimMismatch(
                    f"memory DB {self.path} has embedder dim {row}, embedder "
                    f"{self.embedder.name!r} has dim {dim}; delete/rebuild the DB"
                )
            for t in ("events_vec", "summaries_vec"):
                self.db.execute(
                    f"CREATE VIRTUAL TABLE IF NOT EXISTS {t} USING vec0(embedding float[{int(dim)}])"
                )

    def _meta(self, key: str) -> str | None:
        r = self.db.execute("SELECT value FROM meta WHERE key=?", (key,)).fetchone()
        return None if r is None else str(r["value"])

    def _set_meta(self, key: str, value: str) -> None:
        self.db.execute("INSERT OR REPLACE INTO meta(key,value) VALUES(?,?)", (key, value))

    def close(self) -> None:
        with self._lock:
            self.db.close()

    # --------------------------------------------------------- sessions
    def open_session(self, session_id: str, scenario: str, wall_ms: int | None = None) -> None:
        with self._lock:
            self.db.execute(
                "INSERT OR IGNORE INTO sessions(id,scenario,created_wall_ms) VALUES(?,?,?)",
                (session_id, scenario, wall_ms if wall_ms is not None else int(time.time() * 1000)),
            )

    def close_session(self, session_id: str, wall_ms: int | None = None) -> None:
        with self._lock:
            self.db.execute(
                "UPDATE sessions SET closed_wall_ms=? WHERE id=?",
                (wall_ms if wall_ms is not None else int(time.time() * 1000), session_id),
            )

    def add_transcript(
        self, session_id: str, utterance_id: str, role: str, ts_ms: int, wall_ms: int, text: str
    ) -> None:
        with self._lock:
            self.db.execute(
                "INSERT INTO transcripts(session_id,utterance_id,role,ts_ms,wall_ms,text) VALUES(?,?,?,?,?,?)",
                (session_id, utterance_id, role, ts_ms, wall_ms, text),
            )

    def transcripts(self, session_id: str) -> list[dict[str, Any]]:
        with self._lock:
            rows = self.db.execute(
                "SELECT utterance_id,role,ts_ms,text FROM transcripts WHERE session_id=? ORDER BY rowid",
                (session_id,),
            ).fetchall()
        return [dict(r) for r in rows]

    # ----------------------------------------------------------- writes
    def add_events(self, events: list[SceneEvent]) -> int:
        n = 0
        with self._lock:
            self.db.execute("BEGIN")
            try:
                for e in events:
                    cur = self.db.execute(
                        "INSERT OR IGNORE INTO events(id,stream_id,scenario,ts_ms,wall_ms,type,confidence,summary,"
                        "actors_json,attrs_json,frame_stream,frame_ts_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
                        (
                            e.id, e.stream_id, e.scenario, e.ts_ms, e.wall_ms, e.type, e.confidence, e.summary,
                            json.dumps([a.wire() for a in e.actors]), json.dumps(e.attrs),
                            e.frame_ref.stream_id if e.frame_ref else None,
                            e.frame_ref.ts_ms if e.frame_ref else None,
                        ),
                    )  # fmt: skip
                    n += cur.rowcount
                self.db.execute("COMMIT")
            except Exception:
                self.db.execute("ROLLBACK")
                raise
        return n

    def update_attrs(self, event_id: str, attrs: dict[str, Any]) -> None:
        with self._lock:
            self.db.execute("UPDATE events SET attrs_json=? WHERE id=?", (json.dumps(attrs), event_id))

    def add_summary(self, s: MinuteSummary) -> bool:
        """Insert a minute summary; returns False if one already exists for (stream, minute)."""
        with self._lock:
            cur = self.db.execute(
                "INSERT OR IGNORE INTO minute_summaries(stream_id,minute_start_ms,wall_ms,text,event_count) "
                "VALUES(?,?,?,?,?)",
                (s.stream_id, s.minute_start_ms, s.wall_ms, s.text, s.event_count),
            )
            return cur.rowcount > 0

    def has_summary(self, stream_id: str, minute_start_ms: int) -> bool:
        with self._lock:
            return (
                self.db.execute(
                    "SELECT 1 FROM minute_summaries WHERE stream_id=? AND minute_start_ms=?",
                    (stream_id, minute_start_ms),
                ).fetchone()
                is not None
            )

    async def embed_pending(self, batch: int = 64) -> int:
        """Embed rows lacking vectors (events + summaries). Returns number embedded."""
        total = 0
        for table, vec, text_sql in (
            ("events", "events_vec", "type || ': ' || summary"),
            ("minute_summaries", "summaries_vec", "text"),
        ):
            while True:
                with self._lock:
                    rows = self.db.execute(
                        f"SELECT rowid AS rid, {text_sql} AS t FROM {table} WHERE embedded=0 ORDER BY rowid LIMIT ?",  # noqa: S608
                        (batch,),
                    ).fetchall()
                if not rows:
                    break
                vecs = await self.embedder.embed([r["t"] for r in rows])
                with self._lock:
                    self.db.execute("BEGIN")
                    try:
                        for r, v in zip(rows, vecs, strict=True):
                            self.db.execute(
                                f"INSERT OR REPLACE INTO {vec}(rowid, embedding) VALUES(?,?)",  # noqa: S608
                                (int(r["rid"]), _blob(v)),
                            )
                            self.db.execute(f"UPDATE {table} SET embedded=1 WHERE rowid=?", (int(r["rid"]),))  # noqa: S608
                        self.db.execute("COMMIT")
                    except Exception:
                        self.db.execute("ROLLBACK")
                        raise
                total += len(rows)
        return total

    # ------------------------------------------------------------ reads
    @staticmethod
    def _row_event(r: sqlite3.Row) -> SceneEvent:
        return SceneEvent(
            id=r["id"], stream_id=r["stream_id"], ts_ms=r["ts_ms"], wall_ms=r["wall_ms"], type=r["type"],
            confidence=r["confidence"], summary=r["summary"], scenario=r["scenario"],
            actors=[Actor(**a) for a in json.loads(r["actors_json"])],
            attrs=json.loads(r["attrs_json"]),
            frame_ref=FrameRef(stream_id=r["frame_stream"], ts_ms=r["frame_ts_ms"])
            if r["frame_stream"] is not None and r["frame_ts_ms"] is not None
            else None,
        )  # fmt: skip

    def recent_events(self, stream_id: str, n: int = 5) -> list[SceneEvent]:
        with self._lock:
            rows = self.db.execute(
                "SELECT * FROM events WHERE stream_id=? ORDER BY ts_ms DESC, rowid DESC LIMIT ?",
                (stream_id, n),
            ).fetchall()
        return [self._row_event(r) for r in reversed(rows)]

    def events_in(self, stream_id: str | None, since_ms: int, until_ms: int) -> list[SceneEvent]:
        """Events with ``since_ms <= wall_ms < until_ms``."""
        with self._lock:
            rows = self._events_rows(stream_id, since_ms, until_ms)
        return [self._row_event(r) for r in rows]

    def events_by_ts(self, stream_id: str, since_ts_ms: int, until_ts_ms: int) -> list[SceneEvent]:
        """Events with ``since <= ts_ms < until`` on the session clock (compactor minute bucketing)."""
        with self._lock:
            rows = self.db.execute(
                "SELECT * FROM events WHERE stream_id=? AND ts_ms>=? AND ts_ms<? ORDER BY ts_ms, rowid",
                (stream_id, since_ts_ms, until_ts_ms),
            ).fetchall()
        return [self._row_event(r) for r in rows]

    def _events_rows(self, stream_id: str | None, since_ms: int, until_ms: int) -> list[sqlite3.Row]:
        sql = "SELECT * FROM events WHERE wall_ms>=? AND wall_ms<?"
        args: list[Any] = [since_ms, until_ms]
        if stream_id is not None:
            sql += " AND stream_id=?"
            args.append(stream_id)
        return self.db.execute(sql + " ORDER BY wall_ms, ts_ms, rowid", args).fetchall()

    def _summary_rows(self, stream_id: str | None, since_ms: int, until_ms: int) -> list[sqlite3.Row]:
        sql = "SELECT rowid AS rid, * FROM minute_summaries WHERE wall_ms>=? AND wall_ms<?"
        args: list[Any] = [since_ms, until_ms]
        if stream_id is not None:
            sql += " AND stream_id=?"
            args.append(stream_id)
        return self.db.execute(sql + " ORDER BY wall_ms, rowid", args).fetchall()

    @staticmethod
    def _row_summary(r: sqlite3.Row) -> MinuteSummary:
        return MinuteSummary(
            stream_id=r["stream_id"], minute_start_ms=r["minute_start_ms"], wall_ms=r["wall_ms"],
            text=r["text"], event_count=r["event_count"],
        )  # fmt: skip

    def _knn(self, vec_table: str, rowids: list[int], q: list[float]) -> dict[int, float]:
        """Cosine similarity of the query against stored vectors for ``rowids`` (unit vectors)."""
        if not rowids:
            return {}
        rows = self.db.execute(
            f"SELECT rowid AS rid, vec_distance_cosine(embedding, ?) AS d FROM {vec_table} "  # noqa: S608
            "WHERE rowid IN (SELECT value FROM json_each(?))",
            (_blob(q), json.dumps(rowids)),
        ).fetchall()
        return {int(r["rid"]): 1.0 - float(r["d"]) for r in rows}

    async def search(
        self,
        stream_id: str | None = None,
        *,
        since_ms: int,
        until_ms: int,
        text: str | None = None,
        limit: int = 50,
    ) -> SearchResult:
        """Time-window filter, then (if ``text``) vector top-k re-ranked by ``0.7*cos + 0.3*recency``.

        ``recency`` is the position of ``wall_ms`` inside the window (1 = newest). Without ``text`` the
        newest ``limit`` events are returned in chronological order.
        """
        if until_ms <= since_ms:
            return SearchResult()
        span = float(until_ms - since_ms)

        def recency(ts: int) -> float:
            return min(1.0, max(0.0, (ts - since_ms) / span))

        if text is None or not text.strip():
            with self._lock:
                rows = self._events_rows(stream_id, since_ms, until_ms)
                srows = self._summary_rows(stream_id, since_ms, until_ms)
            truncated = len(rows) > limit
            rows = rows[-limit:] if truncated else rows
            return SearchResult(
                [self._row_event(r) for r in rows], [self._row_summary(r) for r in srows[-limit:]], truncated
            )

        await self.embed_pending()
        q = (await self.embedder.embed([text]))[0]
        with self._lock:
            rows = self._events_rows(stream_id, since_ms, until_ms)
            srows = self._summary_rows(stream_id, since_ms, until_ms)
            cos = self._knn("events_vec", [int(r["rowid"]) for r in rows[-_MAX_CANDIDATES:]], q)
            scos = self._knn("summaries_vec", [int(r["rid"]) for r in srows[-_MAX_CANDIDATES:]], q)
        scored = sorted(
            (
                (W_COS * cos.get(int(r["rowid"]), 0.0) + W_RECENCY * recency(r["wall_ms"]), r)
                for r in rows
                if int(r["rowid"]) in cos
            ),
            key=lambda x: (-x[0], -x[1]["wall_ms"]),
        )
        truncated = len(scored) > limit
        top = scored[:limit]
        sscored = sorted(
            (
                (W_COS * scos.get(int(r["rid"]), 0.0) + W_RECENCY * recency(r["wall_ms"]), r)
                for r in srows
                if int(r["rid"]) in scos
            ),
            key=lambda x: -x[0],
        )[:limit]
        events = [self._row_event(r) for _, r in top]  # ranked best-first
        return SearchResult(
            events,
            [self._row_summary(r) for _, r in sscored],
            truncated,
            {e.id: round(s, 6) for (s, _), e in zip(top, events, strict=True)},
        )

    # -------------------------------------------------------- retention
    def retention_job(self, now_wall_ms: int, retention_s: int | None = None) -> dict[str, int]:
        """Delete rows with ``wall_ms < now - retention`` from base tables *and* vec tables."""
        cutoff = now_wall_ms - (retention_s if retention_s is not None else self.retention_s) * 1000
        out = {"events": 0, "summaries": 0}
        with self._lock:
            self.db.execute("BEGIN")
            try:
                for table, vec, key in (
                    ("events", "events_vec", "events"),
                    ("minute_summaries", "summaries_vec", "summaries"),
                ):
                    self.db.execute(
                        f"DELETE FROM {vec} WHERE rowid IN (SELECT rowid FROM {table} WHERE wall_ms < ?)",  # noqa: S608
                        (cutoff,),
                    )
                    out[key] = self.db.execute(f"DELETE FROM {table} WHERE wall_ms < ?", (cutoff,)).rowcount  # noqa: S608
                self.db.execute("COMMIT")
            except Exception:
                self.db.execute("ROLLBACK")
                raise
        return out

    def counts(self) -> dict[str, int]:
        with self._lock:
            return {
                t: int(self.db.execute(f"SELECT count(*) FROM {t}").fetchone()[0])  # noqa: S608
                for t in ("events", "events_vec", "minute_summaries", "summaries_vec")
            }
