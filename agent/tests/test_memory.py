import uuid

import pytest

from argus.memory.compactor import Compactor
from argus.memory.store import MemoryDimMismatch, MemoryStore
from argus.protocol import MinuteSummary, SceneEvent
from argus.providers.fake import FakeEmbedder


def mk(summary, ts, stream="cam", type_="x.y", wall=None):
    return SceneEvent(
        id=str(uuid.uuid4()),
        stream_id=stream,
        ts_ms=ts,
        wall_ms=wall if wall is not None else 1_000_000 + ts,
        type=type_,
        confidence=0.9,
        summary=summary,
        scenario="home_monitor",
    )


async def test_window_is_half_open_on_wall(store):
    base = 1_000_000
    store.add_events([mk("a", 0), mk("b", 1000), mk("c", 2000), mk("d", 3000)])
    r = await store.search(None, since_ms=base + 1000, until_ms=base + 3000)
    assert [e.summary for e in r.events] == ["b", "c"]  # since inclusive, until exclusive
    assert (await store.search(None, since_ms=base, until_ms=base)).events == []
    r = await store.search("other", since_ms=0, until_ms=base + 10_000)
    assert r.events == []
    r = await store.search("cam", since_ms=0, until_ms=base + 10_000, limit=2)
    assert [e.summary for e in r.events] == ["c", "d"] and r.truncated


async def test_vector_search_ranks_semantic_match_first(store):
    store.add_events(
        [
            mk("A brown dog eats from a bowl in the kitchen", 1000, type_="dog.eating"),
            mk("A person stands at the front door", 2000, type_="person.at_door"),
            mk("A package is left at the door", 3000, type_="package.delivered"),
            mk("The cat sleeps on the sofa", 4000, type_="cat.sleeping"),
        ]
    )
    r = await store.search(None, since_ms=0, until_ms=2_000_000, text="dog eating dinner", limit=3)
    assert r.events[0].type == "dog.eating"
    assert store.counts()["events_vec"] == 4
    r = await store.search(None, since_ms=0, until_ms=2_000_000, text="package delivered at door", limit=3)
    assert r.events[0].type == "package.delivered"


async def test_recency_breaks_ties(store):
    store.add_events([mk("dog eating", 0), mk("dog eating", 100_000)])
    r = await store.search(None, since_ms=0, until_ms=2_000_000, text="dog eating", limit=2)
    assert [e.ts_ms for e in r.events] == [100_000, 0]
    assert r.scores[r.events[0].id] > r.scores[r.events[1].id]


async def test_retention_deletes_vec_rows(store):
    store.add_events([mk("old thing", 0, wall=1_000), mk("new thing", 0, wall=9_000_000)])
    store.add_summary(
        MinuteSummary(stream_id="cam", minute_start_ms=0, wall_ms=1_000, text="old summary", event_count=1)
    )
    await store.embed_pending()
    assert store.counts() == {"events": 2, "events_vec": 2, "minute_summaries": 1, "summaries_vec": 1}
    out = store.retention_job(now_wall_ms=4_000_000, retention_s=3600)  # cutoff = 400_000
    assert out == {"events": 1, "summaries": 1}
    assert store.counts() == {"events": 1, "events_vec": 1, "minute_summaries": 0, "summaries_vec": 0}


def test_dim_mismatch_refused(tmp_path):
    p = tmp_path / "m.db"
    MemoryStore(p, FakeEmbedder(64)).close()
    MemoryStore(p, FakeEmbedder(64)).close()  # same dim fine
    with pytest.raises(MemoryDimMismatch):
        MemoryStore(p, FakeEmbedder(32))


async def test_embed_pending_batches_and_duplicates(store):
    evs = [mk(f"event {i}", i) for i in range(5)]
    assert store.add_events(evs) == 5
    assert store.add_events(evs) == 0  # idempotent on id
    assert await store.embed_pending(batch=2) == 5
    assert await store.embed_pending() == 0


async def test_compactor_keeps_high_severity(store, providers, packs):
    pack = packs.get("home_monitor")
    store.add_events(
        [
            mk("dog eats", 5_000, type_="dog.eating"),
            mk("someone walks in", 20_000, type_="person.entered"),
            mk("next minute", 65_000, type_="dog.sleeping"),
            mk("snapshot", 21_000, type_="state.snapshot"),
        ]
    )
    c = Compactor(store, providers.chat, pack)
    made = await c.compact_stream("cam", now_ts_ms=70_000)
    assert [m.minute_start_ms for m in made] == [0] and made[0].event_count == 2
    assert "Key events" in made[0].text and "someone walks in" in made[0].text
    assert await c.compact_stream("cam", now_ts_ms=70_000) == []  # idempotent
    made = await c.compact_stream("cam", now_ts_ms=70_000, include_open_minute=True)
    assert [m.minute_start_ms for m in made] == [60_000]
    r = await store.search("cam", since_ms=0, until_ms=2_000_000)
    assert len(r.summaries) == 2


async def test_contains_filter_narrows_candidates(store):
    store.add_events(
        [mk("dog eats from the bowl", 1000), mk("person at the door", 2000), mk("it's a dog's day", 3000)]
    )
    r = await store.search(None, since_ms=0, until_ms=2_000_000, contains="dog")
    assert [e.summary for e in r.events] == ["dog eats from the bowl", "it's a dog's day"]
    r = await store.search(None, since_ms=0, until_ms=2_000_000, text="door", contains="person")
    assert [e.summary for e in r.events] == ["person at the door"]
    assert [e.ts_ms for e in store.events_in(None, 0, 2_000_000, order_by="ts_ms")] == [1000, 2000, 3000]
    assert store.events_in(
        None, 0, 2_000_000, order_by="ts_ms; DROP TABLE events"
    )  # unknown column -> wall_ms
