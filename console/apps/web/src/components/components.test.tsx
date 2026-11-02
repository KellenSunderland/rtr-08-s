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
});

describe("Transcript rich text", () => {
  it("renders bold, code and links", () => {
    const text = "see **this** and `that`: [docs](https://example.com/a)";
    const { container } = render(
      <Transcript entries={[{ key: "1", role: "assistant", text, partial: false }]} />,
    );
    expect(container.querySelector("strong")?.textContent).toBe("this");
    expect(container.querySelector("code")?.textContent).toBe("that");
    expect(container.querySelector("a")?.getAttribute("href")).toBe("https://example.com/a");
  });
});
