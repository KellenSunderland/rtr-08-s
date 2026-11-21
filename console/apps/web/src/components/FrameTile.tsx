import { useMemo, useState } from "react";
import { useFrame } from "../hooks";
import { useApp } from "../ctx";
import type { SceneEvent, StreamInfo } from "@argus/protocol";
import { eventForFrame, overlayBoxes } from "../lib/overlay";
import { StatusBadge } from "./StatusBadge";

export function FrameTile({
  stream,
  seekTs,
  maxW = 320,
  events,
}: {
  stream: Pick<StreamInfo, "id" | "name" | "state" | "fps">;
  seekTs?: number | null;
  maxW?: number;
  /** When given, actor boxes of the event matching the shown frame are drawn over it. */
  events?: readonly SceneEvent[];
}) {
  const { api } = useApp();
  const { url, tsMs, error } = useFrame(api, stream.id, seekTs ?? null, maxW);
  const [size, setSize] = useState<{ w: number; h: number } | null>(null);

  const matched = useMemo(
    () => (events && tsMs !== null ? eventForFrame(events, stream.id, tsMs) : null),
    [events, stream.id, tsMs],
  );
  const boxes = useMemo(
    () => (matched && size ? overlayBoxes(matched, size.w, size.h) : []),
    [matched, size],
  );

  return (
    <figure className="tile">
      <div className="tile-img">
        {url ? (
          <div className="frame-box" style={size ? { aspectRatio: `${size.w} / ${size.h}` } : undefined}>
            <img
              src={url}
              alt={`Latest frame of ${stream.name || stream.id}`}
              onLoad={(e) => {
                const { naturalWidth: w, naturalHeight: h } = e.currentTarget;
                if (!size || size.w !== w || size.h !== h) setSize({ w, h });
              }}
            />
            {boxes.map((b) => (
              <span
                key={b.key}
                className="actor-box"
                style={{ left: `${b.left}%`, top: `${b.top}%`, width: `${b.width}%`, height: `${b.height}%` }}
              >
                <span className="actor-label">{b.label}</span>
              </span>
            ))}
          </div>
        ) : (
          <span className="muted">no frame</span>
        )}
        {seekTs != null && <span className="seek-tag">@ {Math.round(seekTs / 1000)}s</span>}
      </div>
      <figcaption>
        <strong>{stream.name || stream.id}</strong> <StatusBadge state={stream.state} fps={stream.fps} />
        {matched && <span className="muted small"> · {matched.type}</span>}
        {error && <div className="err small">{error}</div>}
      </figcaption>
    </figure>
  );
}
