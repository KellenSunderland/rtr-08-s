import { useEffect, useState } from "react";
import type { StreamInfo } from "@argus/protocol";
import { useApp } from "../ctx";
import { useInterval } from "../hooks";
import { useSession } from "../useSession";
import { FrameTile } from "../components/FrameTile";
import { Transcript } from "../components/Transcript";
import { ConnectionBadge } from "../components/ConnectionBadge";
import { Timeline } from "../components/Timeline";
import { EventList } from "../components/EventList";
import { ScenarioPanel } from "../components/ScenarioPanel";

export function SessionPage() {
  const { api, config } = useApp();
  const sess = useSession(api);
  const [available, setAvailable] = useState<StreamInfo[]>([]);
  const [picked, setPicked] = useState<Set<string>>(new Set());
  const [text, setText] = useState("");
  const [seekTs, setSeekTs] = useState<number | null>(null);
  const [nowMs, setNowMs] = useState(() => Date.now());
  useInterval(() => setNowMs(Date.now()), 1000);
  // wall-clock time at session-clock zero (approximate); frames are addressed by session-clock ts_ms
  const wallOffset = () => Date.now() - sess.clock.ms();

  useEffect(() => {
    api
      .listStreams()
      .then(setAvailable)
      .catch(() => setAvailable([]));
  }, [api]);

  const { state } = sess;
  const live = sess.status === "open" || sess.status === "reconnecting" || sess.status === "connecting";
  const tiles = state.streams.length > 0 ? state.streams : available.filter((s) => picked.has(s.id));

  return (
    <div>
      <div className="row">
        {!live &&
          available.map((s) => (
            <label key={s.id}>
              <input
                type="checkbox"
                checked={picked.has(s.id)}
                onChange={() => {
                  const n = new Set(picked);
                  if (n.has(s.id)) n.delete(s.id);
                  else n.add(s.id);
                  setPicked(n);
                }}
              />{" "}
              {s.id}
            </label>
          ))}
        {!live ? (
          <button
            disabled={picked.size === 0 || !config.scenario}
            onClick={() => void sess.start(config.scenario, [...picked])}
          >
            Start session ({config.scenario || "no scenario"})
          </button>
        ) : (
          <button className="danger" onClick={() => void sess.stop()}>
            End session
          </button>
        )}
        <ConnectionBadge />
      </div>
      {sess.error && <p className="err">{sess.error}</p>}
      {state.errors.length > 0 && <p className="err small">{state.errors[state.errors.length - 1]}</p>}

      <div className="grid">
        {tiles.map((s) => (
          <FrameTile key={s.id} stream={s} seekTs={seekTs} />
        ))}
      </div>

      {live && (
        <div className="row controls">
          <button
            className={sess.talking ? "talking" : ""}
            onPointerDown={(e) => {
              e.currentTarget.setPointerCapture(e.pointerId);
              void sess.pttStart();
            }}
            onPointerUp={() => void sess.pttStop()}
            onPointerCancel={() => void sess.pttStop()}
          >
            {sess.talking ? "Listening… (release to send)" : "Hold to talk"}
          </button>
          <button onClick={sess.bargeIn}>Interrupt</button>
          <form
            className="row grow"
            onSubmit={(e) => {
              e.preventDefault();
              sess.sendText(text);
              setText("");
            }}
          >
            <input
              className="grow"
              value={text}
              maxLength={4000}
              placeholder="Type a message"
              onChange={(e) => setText(e.target.value)}
            />
            <button type="submit" disabled={!text.trim()}>
              Send
            </button>
          </form>
        </div>
      )}

      <Timeline
        events={state.events}
        summaries={state.summaries}
        nowMs={nowMs}
        seekTs={seekTs === null ? null : seekTs + wallOffset()}
        onSeek={(wall) => setSeekTs(wall === null ? null : Math.max(0, wall - wallOffset()))}
      />
      <div className="cols">
        <Transcript entries={state.transcript} />
        <ScenarioPanel scenario={state.scenario ?? config.scenario} events={state.events} />
        <EventList events={state.events} onSeek={setSeekTs} />
      </div>
    </div>
  );
}
