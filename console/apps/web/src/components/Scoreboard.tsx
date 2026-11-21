import type { SceneEvent } from "@argus/protocol";
import { useMemo } from "react";
import { latestSnapshot, parseSnapshotScore, reduceScoreboard } from "../lib/scoreboard";

export function Scoreboard({ events }: { events: readonly SceneEvent[] }) {
  const sb = useMemo(() => reduceScoreboard(events), [events]);
  // Prefer the agent's authoritative state.snapshot; fall back to the local reducer.
  const snap = useMemo(() => {
    const l = latestSnapshot(events);
    return l ? parseSnapshotScore(l.state) : null;
  }, [events]);
  const scores = snap ? snap.score : sb.scores;
  const sides = Object.entries(scores).sort(([a], [b]) => a.localeCompare(b));
  return (
    <div className="scoreboard" aria-label="Scoreboard">
      {sides.length === 0 && <span className="muted">No points yet</span>}
      {sides.map(([side, pts]) => (
        <div
          key={side}
          className={`score${(snap ? snap.serving_team : sb.lastWinner) === side ? " last" : ""}`}
        >
          <span className="side">{side}</span>
          <span className="pts">{pts}</span>
        </div>
      ))}
      {snap && (
        <div className="muted small">
          serving {snap.serving_team ?? "?"} #{snap.server_number ?? "?"} · game {snap.game ?? "?"}
          {snap.side_switched ? " · sides switched" : ""}
        </div>
      )}
      {Object.keys(sb.faults).length > 0 && (
        <div className="muted small">
          faults:{" "}
          {Object.entries(sb.faults)
            .sort(([a], [b]) => a.localeCompare(b))
            .map(([side, n]) => `${side} ${n}`)
            .join(" · ")}
        </div>
      )}
      <div className="muted small">
        rallies: {sb.rallies}
        {sb.rallyActive ? " (in play)" : ""}
      </div>
    </div>
  );
}
