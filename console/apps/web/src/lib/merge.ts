import type { SceneEvent } from "@argus/protocol";

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => typeof v === "object" && v !== null && !Array.isArray(v);

/**
 * Recursively merge `patch` into `target` (mutating it): nested objects merge key by key,
 * scalars and arrays in `patch` replace what is in `target`. Returns `target`.
 */
export function mergeInto(target: Obj, patch: Obj): Obj {
  for (const key of Object.keys(patch)) {
    const next = patch[key];
    if (isObj(next)) {
      const cur = target[key];
      mergeInto(isObj(cur) ? cur : (target[key] = {}), next);
    } else {
      target[key] = next;
    }
  }
  return target;
}

/** An `event` message that re-uses an id carries a partial update: its attrs overlay the stored ones. */
export function mergeEventUpdate(prev: SceneEvent, incoming: SceneEvent): SceneEvent {
  return { ...incoming, attrs: mergeInto(structuredClone(prev.attrs), incoming.attrs) };
}
