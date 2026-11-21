import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { SceneEvent } from "@argus/protocol";
import { Transcript } from "./Transcript";
import { Scoreboard } from "./Scoreboard";

describe("Transcript", () => {
  it("renders hostile text as inert text, not HTML", () => {
    const evil = '<img src=x onerror="alert(1)"><script>alert(2)</script>';
    const { container } = render(
      <Transcript entries={[{ key: "1", role: "assistant", text: evil, partial: true }]} />,
    );
    expect(container.querySelector("img")).toBeNull();
    expect(container.querySelector("script")).toBeNull();
    expect(screen.getByText(evil, { exact: false })).toBeInTheDocument();
  });
});

describe("Scoreboard", () => {
  it("shows scores from events", () => {
    const e = (i: number, type: string, team?: string): SceneEvent => ({
      id: `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`,
      stream_id: "c",
      ts_ms: i,
      wall_ms: 1,
      type,
      confidence: 1,
      actors: [],
      attrs: team ? { team } : {},
      summary: "",
      scenario: "pickleball_referee",
    });
    render(<Scoreboard events={[e(1, "rally.start"), e(2, "point.won", "a"), e(3, "point.won", "a")]} />);
    expect(screen.getByText("2")).toBeInTheDocument();
    expect(screen.getByText(/rallies: 1/)).toBeInTheDocument();
  });

  it("shows the fault tally per team and hides it when there are no faults", () => {
    const e = (i: number, type: string, team: string): SceneEvent => ({
      id: `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`,
      stream_id: "c",
      ts_ms: i,
      wall_ms: 1,
      type,
      confidence: 1,
      actors: [],
      attrs: { team },
      summary: "",
      scenario: "pickleball_referee",
    });
    const { rerender } = render(<Scoreboard events={[e(1, "point.won", "A")]} />);
    expect(screen.queryByText(/faults:/)).toBeNull();
    rerender(<Scoreboard events={[e(1, "fault.kitchen", "A"), e(2, "fault.serve", "B"), e(3, "fault.serve", "B")]} />);
    expect(screen.getByText(/faults: A 1 · B 2/)).toBeInTheDocument();
  });
});

