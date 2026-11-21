import { describe, expect, it } from "vitest";
import type { SceneEvent } from "@argus/protocol";
import { OVERLAY_MAX_AGE_MS, eventForFrame, intHeader, overlayBoxes } from "./overlay";

const ev = (i: number, stream_id: string, ts_ms: number, actors: SceneEvent["actors"]): SceneEvent => ({
  id: `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`,
  stream_id,
  ts_ms,
  wall_ms: 1,
  type: "person.enter",
  confidence: 0.9,
  actors,
  attrs: {},
  summary: "",
  scenario: "home_monitor",
});

const person = { label: "person", track_id: "t1", bbox: { x: 160, y: 90, w: 320, h: 180 } };

describe("eventForFrame", () => {
  it("matches an event of the same stream shortly before the frame", () => {
    const events = [ev(1, "cam1", 10_000, [person]), ev(2, "cam2", 10_500, [person])];
    expect(eventForFrame(events, "cam1", 10_400)?.id).toBe(events[0]!.id);
    expect(eventForFrame(events, "cam2", 10_400)).toBeNull(); // after the frame
  });
  it("ignores events that are too old or have no boxes", () => {
    const events = [ev(1, "cam1", 10_000, [person]), ev(2, "cam1", 20_000, [{ label: "dog" }])];
    expect(eventForFrame(events, "cam1", 10_000 + OVERLAY_MAX_AGE_MS)).not.toBeNull();
    expect(eventForFrame(events, "cam1", 10_001 + OVERLAY_MAX_AGE_MS)).toBeNull();
    expect(eventForFrame(events, "cam1", 20_100)).toBeNull();
  });
});

describe("overlayBoxes", () => {
  it("converts boxes to percentages of the image", () => {
    const [b] = overlayBoxes(ev(1, "cam1", 0, [{ ...person, confidence: 0.87 }]), 640, 360);
    expect(b).toEqual({ key: "t1", label: "person 87%", left: 25, top: 25, width: 50, height: 50 });
  });
  it("clamps boxes to the frame and skips actors without a box", () => {
    const boxes = overlayBoxes(
      ev(1, "cam1", 0, [{ label: "ball", bbox: { x: 600, y: 300, w: 100, h: 100 } }, { label: "dog" }]),
      640,
      360,
    );
    expect(boxes).toHaveLength(1);
    expect(boxes[0]!.left + boxes[0]!.width).toBeCloseTo(100);
    expect(boxes[0]!.top + boxes[0]!.height).toBeCloseTo(100);
    expect(overlayBoxes(ev(1, "cam1", 0, [person]), 0, 0)).toEqual([]);
  });
});

describe("intHeader", () => {
  it("parses integers and rejects junk", () => {
    const h = new Headers({ "X-Frame-Ts-Ms": "1234", "X-Bad": "1.5", "X-Empty": "" });
    expect(intHeader(h, "x-frame-ts-ms")).toBe(1234);
    expect(intHeader(h, "X-Bad")).toBeNull();
    expect(intHeader(h, "X-Empty")).toBeNull();
    expect(intHeader(h, "X-Missing")).toBeNull();
  });
});
