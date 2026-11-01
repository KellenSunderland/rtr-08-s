from dataclasses import replace

from argus.metrics import Metrics
from argus.perception.limiter import EventLimiter
from argus.protocol import SceneEvent


def ev(ts, etype="dog.sleeping", conf=0.9, wall=None):
    return SceneEvent(
        id=f"{etype}-{ts}-{conf}", stream_id="cam", ts_ms=ts, wall_ms=wall or 1_000 + ts, type=etype,
        confidence=conf, summary="s", scenario="home_monitor",
    )  # fmt: skip


def mk(packs, metrics=None, **sampling):
    pack = packs.get("home_monitor")
    pack = replace(pack, sampling=replace(pack.sampling, **sampling))
    return EventLimiter(pack, "cam", metrics or Metrics())


def actions(lim, *events):
    return [d.action for d in lim.process(list(events))]


def test_cooldown_collapses_into_first(packs):
    lim = mk(packs)
    first = ev(0)
    ds = lim.process([first, ev(3_000, wall=4_000)])
    assert [d.action for d in ds] == ["emit", "collapse"]
    assert ds[1].target is first
    assert first.attrs == {"repeat_count": 2, "last_seen_wall_ms": 4_000}


def test_cooldown_is_sliding_and_expires(packs):
    lim = mk(packs, event_cooldown_s=30)
    assert actions(lim, ev(0)) == ["emit"]
    for t in range(10_000, 100_000, 10_000):  # each within 30 s of the previous sighting
        assert actions(lim, ev(t)) == ["collapse"]
    assert actions(lim, ev(131_000)) == ["emit"]  # gap > 30 s starts a new event


def test_other_types_not_collapsed(packs):
    lim = mk(packs)
    assert actions(lim, ev(0), ev(100, "dog.eating")) == ["emit", "emit"]


def test_per_type_override_and_disable(packs):
    lim = mk(packs, cooldowns={"dog.sleeping": 1.0})
    assert actions(lim, ev(0), ev(2_000), ev(2_500, "dog.eating"), ev(3_000, "dog.eating")) == [
        "emit", "emit", "emit", "collapse",
    ]  # fmt: skip
    off = mk(packs, event_cooldown_s=0)
    assert actions(off, ev(0), ev(1)) == ["emit", "emit"]


def test_exemptions(packs):
    lim = mk(packs, max_events_per_minute=1)
    # person.entered is severity high in home_monitor; state.snapshot is always exempt
    assert actions(lim, ev(0, "person.entered"), ev(1, "person.entered"), ev(2, "state.snapshot"), ev(3, "state.snapshot")) == [
        "emit", "emit", "emit", "emit",
    ]  # fmt: skip


def test_cap_drops_lowest_confidence_first(packs):
    m = Metrics()
    lim = mk(packs, m, max_events_per_minute=2)
    batch = [ev(0, "dog.eating", 0.3), ev(1, "dog.sleeping", 0.9), ev(2, "person.at_door", 0.6)]
    ds = lim.process(batch)
    assert [d.action for d in ds] == ["drop", "emit", "emit"]
    assert m.get("argus_events_rate_limited_total", stream="cam", reason="cap") == 1


def test_cap_window_slides(packs):
    lim = mk(packs, event_cooldown_s=0, max_events_per_minute=2)
    assert actions(lim, ev(0), ev(1)) == ["emit", "emit"]
    assert actions(lim, ev(2)) == ["drop"]
    assert actions(lim, ev(61_000)) == ["emit"]


def test_dropped_event_followers_dropped_not_tracked(packs):
    lim = mk(packs, max_events_per_minute=1)
    keep = ev(0, "dog.eating", 0.9)
    low = ev(1, "dog.sleeping", 0.1)
    ds = lim.process([keep, low, ev(2, "dog.sleeping", 0.1)])
    assert [d.action for d in ds] == ["emit", "drop", "drop"]
    assert actions(lim, ev(70_000, "dog.sleeping")) == ["emit"]  # not collapsed into a never-emitted event


def test_repeat_thresholds(packs):
    lim = mk(packs, max_events_per_minute=10_000)
    first = ev(0)
    lim.process([first])
    crossed = []
    for i in range(1, 1001):
        (d,) = lim.process([ev(i * 100)])
        if d.crossed:
            crossed.append(first.attrs["repeat_count"])
    assert crossed == [10, 100, 1000]
