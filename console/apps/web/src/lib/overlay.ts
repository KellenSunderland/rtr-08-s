import type { SceneEvent } from "@argus/protocol";

/** An event older than this (relative to the frame) is not drawn on it. */
export const OVERLAY_MAX_AGE_MS = 1500;

/** A box in CSS percentages of the rendered frame. */
export interface OverlayBox {
  key: string;
  label: string;
  left: number;
  top: number;
  width: number;
  height: number;
}

/**
 * The latest event of `streamId` at or before the frame timestamp that carries at least one actor
 * box, or null if there is none within OVERLAY_MAX_AGE_MS of the frame.
 */
export function eventForFrame(
  events: readonly SceneEvent[],
  streamId: string,
  frameTsMs: number,
): SceneEvent | null {
  const hit = events.find(
    (e) => e.stream_id === streamId && e.ts_ms <= frameTsMs && e.actors.some((a) => a.bbox),
  );
  if (!hit || frameTsMs - hit.ts_ms > OVERLAY_MAX_AGE_MS) return null;
  return hit;
}

const pct = (v: number) => Math.min(100, Math.max(0, v * 100));

/** Actor boxes of `e` positioned over a frame rendered from an image of `imgW` x `imgH`. */
export function overlayBoxes(e: SceneEvent, imgW: number, imgH: number): OverlayBox[] {
  if (imgW <= 0 || imgH <= 0) return [];
  const out: OverlayBox[] = [];
  e.actors.forEach((a, i) => {
    if (!a.bbox) return;
    const left = pct(a.bbox.x / imgW);
    const top = pct(a.bbox.y / imgH);
    out.push({
      key: a.track_id ?? `${a.label}-${i}`,
      label: a.confidence !== undefined ? `${a.label} ${Math.round(a.confidence * 100)}%` : a.label,
      left,
      top,
      width: Math.min(100 - left, pct(a.bbox.w / imgW)),
      height: Math.min(100 - top, pct(a.bbox.h / imgH)),
    });
  });
  return out;
}

/** Parse an integer response header (e.g. `X-Frame-Ts-Ms`); null when absent or malformed. */
export function intHeader(h: Headers, name: string): number | null {
  const v = h.get(name);
  if (v === null || v.trim() === "") return null;
  const n = Number(v);
  return Number.isInteger(n) ? n : null;
}
