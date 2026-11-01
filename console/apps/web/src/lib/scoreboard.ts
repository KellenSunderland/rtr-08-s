import type { SceneEvent } from "@argus/protocol";

export interface Scoreboard {
  /** Points per side/team label (normalized to upper-case, e.g. "A"; "unknown" if the event names none). */
  scores: Record<string, number>;
  rallies: number;
  rallyActive: boolean;
  lastPointTs?: number;
  lastWinner?: string;
}

export const emptyScoreboard: Scoreboard = { scores: {}, rallies: 0, rallyActive: false };

/**
 * Label of the side an event names: the first of `attrs.team`, `winner`, `side` or `player` that is a
 * non-empty string (trimmed, upper-cased) or a number. Returns "unknown" when none is present.
 */
export function winnerOf(e: Pick<SceneEvent, "attrs">): string {
  const a = e.attrs;
  for (const k of ["team", "winner", "side", "player"]) {
    const v = a[k];
    if (typeof v === "string" && v.trim()) return v.trim().toUpperCase();
    if (typeof v === "number") return String(v);
  }
  return "unknown";
}

/** Fold one event into the scoreboard. Pure: returns a new value, and event types it does not score are ignored. */
export function applyScoreEvent(s: Scoreboard, e: SceneEvent): Scoreboard {
  if (e.type === "rally.start") return { ...s, rallies: s.rallies + 1, rallyActive: true };
  if (e.type === "point.won") {
    const w = winnerOf(e);
    return {
      ...s,
      scores: { ...s.scores, [w]: (s.scores[w] ?? 0) + 1 },
      rallyActive: false,
      lastPointTs: e.ts_ms,
      lastWinner: w,
    };
  }
  return s;
}

/** Reduce events in `ts_ms` order, de-duplicating by id (history seeding and live events may overlap). */
export function reduceScoreboard(events: readonly SceneEvent[]): Scoreboard {
  const seen = new Set<string>();
  return [...events]
    .sort((a, b) => a.ts_ms - b.ts_ms)
    .reduce((s, e) => {
      if (seen.has(e.id)) return s;
      seen.add(e.id);
      return applyScoreEvent(s, e);
    }, emptyScoreboard);
}

/** attrs of the most recent `state.snapshot` event, if any. */
export function latestSnapshot(
  events: readonly SceneEvent[],
): { ts_ms: number; state: Record<string, unknown> } | null {
  let best: SceneEvent | null = null;
  for (const e of events) if (e.type === "state.snapshot" && (!best || e.ts_ms >= best.ts_ms)) best = e;
  return best ? { ts_ms: best.ts_ms, state: best.attrs } : null;
}

/** Scoreboard state emitted by the agent in `state.snapshot.attrs`; null if shape doesn't match. */
export interface SnapshotScore {
  score: Record<string, number>;
  serving_team?: string;
  server_number?: number;
  game?: number;
  side_switched?: boolean;
}
export function parseSnapshotScore(state: Record<string, unknown>): SnapshotScore | null {
  const sc = state.score;
  if (!sc || typeof sc !== "object") return null;
  const score: Record<string, number> = {};
  for (const [k, v] of Object.entries(sc)) if (typeof v === "number") score[k] = v;
  if (Object.keys(score).length === 0) return null;
  return {
    score,
    serving_team: typeof state.serving_team === "string" ? state.serving_team : undefined,
    server_number: typeof state.server_number === "number" ? state.server_number : undefined,
    game: typeof state.game === "number" ? state.game : undefined,
    side_switched: typeof state.side_switched === "boolean" ? state.side_switched : undefined,
  };
}
