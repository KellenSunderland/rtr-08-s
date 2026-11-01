import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import type { SceneEvent } from "@argus/protocol";
import { Transcript } from "./Transcript";
import { Scoreboard } from "./Scoreboard";
import { Timeline } from "./Timeline";

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
});

describe("Timeline", () => {
  const NOW = 1_700_000_000_000;
  const e = (i: number, wall_ms: number): SceneEvent => ({
    id: `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`,
    stream_id: "c",
    ts_ms: i,
    wall_ms,
    type: "a.b",
    confidence: 1,
    actors: [],
    attrs: {},
    summary: "",
    scenario: "s",
  });
  it("switches zoom level and re-renders the heat strip", () => {
    const events = [e(1, NOW - 10_000), e(2, NOW - 12_000), e(3, NOW - 40 * 60_000)];
    const { container } = render(
      <Timeline events={events} summaries={[]} nowMs={NOW} seekTs={null} onSeek={() => {}} />,
    );
    expect(screen.getByText(/last 1h/)).toBeInTheDocument();
    expect(container.querySelectorAll(".heat .cell")).toHaveLength(30);
    expect(container.querySelectorAll(".heat .cell.h4")).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "15m" }));
    expect(screen.getByText(/last 15m/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "15m" })).toHaveAttribute("aria-pressed", "true");
    expect(container.querySelectorAll(".heat .cell:not(.h0)")).toHaveLength(1);
  });
});
