import uuid

import pytest

from argus.protocol import Actor, SceneEvent
from argus.scenarios.base import NullState, ToolContext

WALL0 = 1_000  # SessionClock(lambda: 1_000): wall = 1_000 + ts


def ev(ts, type_, summary="s", stream="kitchen1", actors=()):
    return SceneEvent(
        id=str(uuid.uuid4()),
        stream_id=stream,
        ts_ms=ts,
        wall_ms=WALL0 + ts,
        type=type_,
        confidence=0.9,
        summary=summary,
        actors=[Actor(label=a) for a in actors],
        scenario="home_monitor",
    )


@pytest.fixture
def tools(packs):
    return {t.name: t.handler for t in packs.get("home_monitor").tools}


def ctx_at(store, now_ts):
    return ToolContext("s", "home_monitor", ["kitchen1", "door1"], NullState(), store, lambda: now_ts)


async def test_last_seen(tools, store):
    store.add_events(
        [
            ev(10_000, "person.at_door", "A courier at the door.", stream="door1", actors=["person"]),
            ev(95_000, "dog.eating", "The dog eats from its bowl."),
        ]
    )
    ctx = ctx_at(store, 120_000)
    out = await tools["last_seen"](ctx, label="dog")
    assert out == "The dog was last seen on kitchen1 at 1:35: The dog eats from its bowl."
    assert "door1 at 0:10" in await tools["last_seen"](ctx, label="person")
    assert await tools["last_seen"](ctx, label="cat") == "No cat seen in the last 60 minutes."
    assert await tools["last_seen"](ctx, label="dog", minutes=0.2) == "No dog seen in the last 0.2 minutes."
    with pytest.raises(ValueError):
        await tools["last_seen"](ctx, label="unicorn")
    with pytest.raises(ValueError):
        await tools["last_seen"](ctx, label="dog", minutes=120)


async def test_activity_counts_and_summary(tools, store):
    store.add_events(
        [
            ev(5_000, "person.at_door", stream="door1"),
            ev(6_000, "person.entered", stream="door1"),
            ev(30_000, "dog.eating"),
            ev(40_000, "dog.eating"),
            ev(41_000, "state.snapshot"),
        ]
    )
    ctx = ctx_at(store, 60_000)
    out = await tools["activity"](ctx)
    assert out == "In the last 15 minutes: dog.eating x2, person.at_door x1, person.entered x1."
    assert await tools["activity"](ctx, stream_id="door1") == (
        "In the last 15 minutes: person.at_door x1, person.entered x1."
    )
    assert (
        await tools["activity"](ctx_at(store, 60_000), minutes=0.1)
        == "Nothing happened in the last 0.1 minutes."
    )
    with pytest.raises(ValueError):
        await tools["activity"](ctx, stream_id="garage")


async def test_activity_includes_latest_summary(tools, store):
    from argus.protocol import MinuteSummary

    store.add_events([ev(65_000, "dog.sleeping")])
    store.add_summary(
        MinuteSummary(
            stream_id="kitchen1",
            minute_start_ms=60_000,
            wall_ms=WALL0 + 65_000,
            text="Dog naps.",
            event_count=1,
        )
    )
    out = await tools["activity"](ctx_at(store, 90_000), minutes=5)
    assert out == "In the last 5 minutes: dog.sleeping x1. Latest summary: Dog naps."
