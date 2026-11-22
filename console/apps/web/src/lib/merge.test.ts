import { describe, expect, it } from "vitest";
import type { SceneEvent } from "@argus/protocol";
import { mergeEventUpdate, mergeInto } from "./merge";

const ev = (attrs: Record<string, unknown>): SceneEvent => ({
  id: "00000000-0000-4000-8000-000000000001",
  stream_id: "c",
  ts_ms: 1,
  wall_ms: 1,
  type: "point.won",
  confidence: 1,
  actors: [],
  attrs,
  summary: "",
  scenario: "s",
});

describe("mergeInto", () => {
  it("merges nested objects and replaces scalars and arrays", () => {
    const t = { a: { x: 1, y: 2 }, l: [1, 2], s: "old" };
    mergeInto(t, { a: { y: 3, z: 4 }, l: [9], s: "new" });
    expect(t).toEqual({ a: { x: 1, y: 3, z: 4 }, l: [9], s: "new" });
  });
});

describe("mergeEventUpdate", () => {
  it("overlays incoming attrs on the stored ones without mutating the stored event", () => {
    const prev = ev({ team: "A", score: { a: 1, b: 0 } });
    const next = mergeEventUpdate(prev, ev({ score: { b: 1 }, reason: "fault" }));
    expect(next.attrs).toEqual({ team: "A", score: { a: 1, b: 1 }, reason: "fault" });
    expect(prev.attrs).toEqual({ team: "A", score: { a: 1, b: 0 } });
  });
});
