import { describe, expect, it } from "vitest";
import type { SceneEvent } from "@argus/protocol";
import {
  MAX_EVENTS,
  STRIP_CELLS,
  bucketByMinute,
  densityStrip,
  filterByType,
  formatClock,
  fromFraction,
  heatLevel,
  inWindow,
  lastHour,
  loadDensityCache,
  mergeEvents,
  toFraction,
  zoomWindow,
} from "./timeline";

const ev = (i: number, ts_ms: number, type = "a.b"): SceneEvent => ({
  id: `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`,
  stream_id: "c",
  ts_ms,
  wall_ms: 1,
  type,
  confidence: 1,
  actors: [],
  attrs: {},
  summary: "",
  scenario: "s",
});

describe("window math", () => {
  it("lastHour clamps at 0", () => {
    expect(lastHour(30_000)).toEqual({ startMs: 0, endMs: 30_000 });
    expect(lastHour(4_000_000)).toEqual({ startMs: 400_000, endMs: 4_000_000 });
  });
  it("start inclusive, end exclusive", () => {
    const w = { startMs: 100, endMs: 200 };
    expect(inWindow(99, w)).toBe(false);
    expect(inWindow(100, w)).toBe(true);
    expect(inWindow(199, w)).toBe(true);
    expect(inWindow(200, w)).toBe(false);
  });
  it("fraction conversion", () => {
    const w = { startMs: 1000, endMs: 3000 };
    expect(toFraction(1000, w)).toBe(0);
    expect(toFraction(2000, w)).toBe(0.5);
    expect(toFraction(9999, w)).toBe(1);
    expect(toFraction(0, w)).toBe(0);
    expect(fromFraction(0.5, w)).toBe(2000);
    expect(fromFraction(1, w)).toBe(2999);
    expect(fromFraction(-1, w)).toBe(1000);
    expect(toFraction(5, { startMs: 5, endMs: 5 })).toBe(0);
  });
});

describe("bucketByMinute", () => {
  it("aligns buckets to session-clock minutes", () => {
    const b = bucketByMinute([], { startMs: 90_000, endMs: 200_000 });
    expect(b.map((x) => x.startMs)).toEqual([60_000, 120_000, 180_000]);
  });
  it("end is exclusive: an event on a minute boundary at end is excluded and adds no bucket", () => {
    const w = { startMs: 0, endMs: 120_000 };
    const b = bucketByMinute([ev(1, 119_999), ev(2, 120_000), ev(3, 0)], w);
    expect(b).toHaveLength(2);
    expect(b.map((x) => x.count)).toEqual([1, 1]);
  });
  it("ignores events before start, counts types and summaries", () => {
    const w = { startMs: 60_000, endMs: 180_000 };
    const b = bucketByMinute([ev(1, 59_999), ev(2, 60_000, "x.y"), ev(3, 61_000, "x.y"), ev(4, 130_000)], w, [
      { stream_id: "c", minute_start_ms: 120_000, wall_ms: 1, text: "t", event_count: 1 },
      { stream_id: "c", minute_start_ms: 0, wall_ms: 1, text: "old", event_count: 1 },
    ]);
    expect(b[0]!.byType).toEqual({ "x.y": 2 });
    expect(b[1]!.count).toBe(1);
    expect(b[1]!.summaries).toHaveLength(1);
  });
  it("empty window", () => expect(bucketByMinute([], { startMs: 5, endMs: 5 })).toEqual([]));
  it("a full hour has 60 buckets", () => expect(bucketByMinute([], lastHour(7_200_000))).toHaveLength(60));
});

describe("event list helpers", () => {
  it("filters by type, empty set = all", () => {
    const l = [ev(1, 1, "a.b"), ev(2, 2, "c.d")];
    expect(filterByType(l, new Set())).toHaveLength(2);
    expect(filterByType(l, new Set(["c.d"]))).toEqual([l[1]]);
  });
  it("merge dedupes, sorts, and caps keeping newest", () => {
    const many = Array.from({ length: MAX_EVENTS + 5 }, (_, i) => ev(i + 1, i));
    const m = mergeEvents([many[3]!], many);
    expect(m).toHaveLength(MAX_EVENTS);
    expect(m[0]!.ts_ms).toBe(5);
    expect(m.at(-1)!.ts_ms).toBe(MAX_EVENTS + 4);
  });
  it("formatClock", () => expect(formatClock(125_000)).toBe("02:05"));
});

describe("zoom levels", () => {
  it("windows trail now by the zoom span", () => {
    const now = 10_000_000;
    expect(zoomWindow(now, "15m")).toEqual({ startMs: now - 900_000, endMs: now });
    expect(zoomWindow(now, "30m")).toEqual({ startMs: now - 1_800_000, endMs: now });
    expect(zoomWindow(now, "1h")).toEqual(lastHour(now));
  });
  it("clamps at zero like lastHour", () =>
    expect(zoomWindow(5_000, "15m")).toEqual({ startMs: 0, endMs: 5_000 }));
});

describe("densityStrip", () => {
  const w = { startMs: 0, endMs: 900_000 }; // 30 cells of 30 s
  it("has a fixed number of cells and an empty window yields zeros", () => {
    expect(densityStrip([], w)).toHaveLength(STRIP_CELLS);
    expect(densityStrip([ev(1, 5)], { startMs: 5, endMs: 5 }).every((c) => c === 0)).toBe(true);
  });
  it("counts events into the cell that contains them", () => {
    const c = densityStrip([ev(1, 10_000), ev(2, 20_000), ev(3, 455_000), ev(4, 899_000)], w);
    expect(c[0]).toBe(2);
    expect(c[15]).toBe(1);
    expect(c[29]).toBe(1);
    expect(c.reduce((a, b) => a + b, 0)).toBe(4);
  });
  it("ignores events outside the window", () => {
    expect(densityStrip([ev(1, 900_001)], w).every((c) => c === 0)).toBe(true);
    expect(densityStrip([ev(1, 100_000)], { startMs: 200_000, endMs: 1_100_000 }).every((c) => c === 0)).toBe(
      true,
    );
  });
  it("heat levels span 0..4", () => {
    expect(heatLevel(0, 10)).toBe(0);
    expect(heatLevel(1, 10)).toBe(1);
    expect(heatLevel(10, 10)).toBe(4);
    expect(heatLevel(3, 0)).toBe(0);
  });
});

describe("density cache", () => {
  const store = (raw: string | null) => ({ getItem: () => raw });
  it("round-trips a valid snapshot", () => {
    const snap = { zoom: "30m", endMs: 5, counts: [1, 2] };
    expect(loadDensityCache(store(JSON.stringify(snap)))).toEqual(snap);
  });
  it("returns null when absent", () => expect(loadDensityCache(store(null))).toBeNull());
});
