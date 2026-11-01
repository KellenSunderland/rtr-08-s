import type { MinuteSummary, SceneEvent } from "@argus/protocol";

export const MINUTE_MS = 60_000;
export const WINDOW_MS = 60 * MINUTE_MS;
export const MAX_EVENTS = 2000;

/** Half-open window on the session clock: `startMs` inclusive, `endMs` exclusive. */
export interface TimeWindow {
  startMs: number;
  endMs: number;
}

/** The last hour ending at `nowMs`; clamped so startMs >= 0 (session clock starts at 0). */
export function lastHour(nowMs: number): TimeWindow {
  const endMs = Math.max(0, Math.floor(nowMs));
  return { startMs: Math.max(0, endMs - WINDOW_MS), endMs };
}

/** start <= ts < end */
export function inWindow(ts: number, w: TimeWindow): boolean {
  return ts >= w.startMs && ts < w.endMs;
}

/** Fraction [0,1) of `ts` across the window; clamped. Empty window -> 0. */
export function toFraction(ts: number, w: TimeWindow): number {
  const span = w.endMs - w.startMs;
  if (span <= 0) return 0;
  return Math.min(1, Math.max(0, (ts - w.startMs) / span));
}

/** Inverse of toFraction; result is an integer ms inside [start, end-1] (or start if empty). */
export function fromFraction(f: number, w: TimeWindow): number {
  const span = w.endMs - w.startMs;
  if (span <= 0) return w.startMs;
  const ts = w.startMs + Math.round(Math.min(1, Math.max(0, f)) * span);
  return Math.min(ts, Math.max(w.startMs, w.endMs - 1));
}

export interface Bucket {
  /** Minute-aligned start (multiple of 60000 on the session clock). Buckets may begin before window.startMs. */
  startMs: number;
  count: number;
  byType: Record<string, number>;
  summaries: MinuteSummary[];
}

/**
 * Bucket events (and summaries by `minute_start_ms`) into minute-aligned buckets covering the window.
 * Buckets run from floor(start/60000) to floor((end-1)/60000) inclusive; items outside the window
 * are ignored. An empty window yields no buckets.
 */
export function bucketByMinute(
  events: readonly Pick<SceneEvent, "ts_ms" | "type">[],
  w: TimeWindow,
  summaries: readonly MinuteSummary[] = [],
): Bucket[] {
  if (w.endMs <= w.startMs) return [];
  const first = Math.floor(w.startMs / MINUTE_MS);
  const last = Math.floor((w.endMs - 1) / MINUTE_MS);
  const buckets: Bucket[] = [];
  for (let m = first; m <= last; m++)
    buckets.push({ startMs: m * MINUTE_MS, count: 0, byType: {}, summaries: [] });
  for (const e of events) {
    if (!inWindow(e.ts_ms, w)) continue;
    const b = buckets[Math.floor(e.ts_ms / MINUTE_MS) - first];
    if (!b) continue;
    b.count++;
    b.byType[e.type] = (b.byType[e.type] ?? 0) + 1;
  }
  for (const s of summaries) {
    const b = buckets[Math.floor(s.minute_start_ms / MINUTE_MS) - first];
    if (b) b.summaries.push(s);
  }
  return buckets;
}

export type ZoomLevel = "15m" | "30m" | "1h";
export const ZOOM_LEVELS: readonly ZoomLevel[] = ["15m", "30m", "1h"];
const ZOOM_SPAN_MS: Record<ZoomLevel, number> = {
  "15m": 15 * MINUTE_MS,
  "30m": 30 * MINUTE_MS,
  "1h": WINDOW_MS,
};
/** The heat strip always has this many cells, whatever the zoom level. */
export const STRIP_CELLS = 30;

/** The trailing window for a zoom level, ending at `nowMs` (same clamping as `lastHour`). */
export function zoomWindow(nowMs: number, zoom: ZoomLevel): TimeWindow {
  const endMs = Math.max(0, Math.floor(nowMs));
  return { startMs: Math.max(0, endMs - ZOOM_SPAN_MS[zoom]), endMs };
}

/**
 * Event counts per heat-strip cell across the window (`cells` equal-width cells, oldest first).
 * Unlike `inWindow`, the right edge is inclusive here: an event stamped exactly at `endMs`
 * (the newest possible one) is counted in the last cell instead of being dropped.
 */
export function densityStrip(
  events: readonly Pick<SceneEvent, "ts_ms">[],
  w: TimeWindow,
  cells = STRIP_CELLS,
): number[] {
  const counts = new Array<number>(cells).fill(0);
  const span = w.endMs - w.startMs;
  if (span <= 0) return counts;
  const cellMs = span / cells;
  for (const e of events) {
    if (e.ts_ms < w.startMs || e.ts_ms > w.endMs) continue;
    const fromEnd = Math.floor((w.endMs - e.ts_ms) / cellMs);
    const idx = cells - 1 - fromEnd;
    if (idx >= 0) counts[idx] = (counts[idx] ?? 0) + 1;
  }
  return counts;
}

/** Heat level 0..4 for a cell: 0 only when empty, 4 at the busiest cell. */
export function heatLevel(count: number, max: number): number {
  if (count <= 0 || max <= 0) return 0;
  return Math.min(4, Math.max(1, Math.ceil((count / max) * 4)));
}

export interface DensityCache {
  zoom: ZoomLevel;
  endMs: number;
  counts: number[];
}
const DENSITY_KEY = "argus.console.density";

/** Last strip we rendered, so a reload shows something before `history.result` arrives. */
export function loadDensityCache(storage: Pick<Storage, "getItem"> = localStorage): DensityCache | null {
  let cache: DensityCache | null = null;
  try {
    const raw = storage.getItem(DENSITY_KEY);
    if (raw) {
      const p = JSON.parse(raw) as DensityCache;
      if (Array.isArray(p.counts) && ZOOM_LEVELS.includes(p.zoom)) cache = p;
    }
  } catch {
    cache = null;
  }
  return cache;
}

export function saveDensityCache(c: DensityCache, storage: Pick<Storage, "setItem"> = localStorage): void {
  storage.setItem(DENSITY_KEY, JSON.stringify(c));
}

/** Empty `types` set means "no filter". */
export function filterByType<T extends { type: string }>(
  items: readonly T[],
  types: ReadonlySet<string>,
): T[] {
  return types.size === 0 ? [...items] : items.filter((i) => types.has(i.type));
}

/** Insert events by id (dedupe, later wins), sort by ts_ms then id, keep the newest `cap`. */
export function mergeEvents(
  existing: readonly SceneEvent[],
  incoming: readonly SceneEvent[],
  cap = MAX_EVENTS,
): SceneEvent[] {
  const byId = new Map<string, SceneEvent>();
  for (const e of existing) byId.set(e.id, e);
  for (const e of incoming) byId.set(e.id, e);
  const all = [...byId.values()].sort((a, b) => a.ts_ms - b.ts_ms || a.id.localeCompare(b.id));
  return all.length > cap ? all.slice(all.length - cap) : all;
}

export function mergeSummaries(
  existing: readonly MinuteSummary[],
  incoming: readonly MinuteSummary[],
): MinuteSummary[] {
  const byKey = new Map<string, MinuteSummary>();
  for (const s of [...existing, ...incoming]) byKey.set(`${s.stream_id}@${s.minute_start_ms}`, s);
  return [...byKey.values()].sort((a, b) => a.minute_start_ms - b.minute_start_ms);
}

export function formatClock(ms: number): string {
  const s = Math.floor(ms / 1000);
  const m = Math.floor(s / 60);
  return `${String(m).padStart(2, "0")}:${String(s % 60).padStart(2, "0")}`;
}
