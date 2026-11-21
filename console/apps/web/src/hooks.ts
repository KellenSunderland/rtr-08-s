import { useEffect, useRef, useState } from "react";
import { ApiClient } from "./lib/api";
import { backoffDelay } from "./lib/backoff";

export const FRAME_POLL_MS = 500;

/**
 * Live thumbnail: polls the frame endpoint every 500 ms (backoff on error). When `seekTs` is a number,
 * polling pauses and a single frame at that session-clock timestamp is shown.
 * Returns a blob: URL (the endpoint needs the bearer header, so <img src> cannot hit it directly) and the
 * timestamp of the frame shown (`X-Frame-Ts-Ms`, null if the gateway did not send it).
 */
export function useFrame(api: ApiClient, streamId: string, seekTs: number | null, maxW = 320) {
  const [url, setUrl] = useState<string | null>(null);
  const [tsMs, setTsMs] = useState<number | null>(null);
  const [error, setError] = useState<string | null>(null);
  const current = useRef<string | null>(null);

  useEffect(() => {
    const ctl = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    let failures = 0;
    const show = ({ blob, tsMs }: { blob: Blob; tsMs: number | null }) => {
      const next = URL.createObjectURL(blob);
      if (current.current) URL.revokeObjectURL(current.current);
      current.current = next;
      setUrl(next);
      setTsMs(tsMs);
    };
    const tick = async () => {
      let delay = FRAME_POLL_MS;
      try {
        show(
          await api.fetchFrameWithTs(
            streamId,
            { max_w: maxW, ...(seekTs !== null ? { ts_ms: seekTs } : {}) },
            ctl.signal,
          ),
        );
        failures = 0;
        setError(null);
      } catch (e) {
        if (ctl.signal.aborted) return;
        setError(e instanceof Error ? e.message : String(e));
        delay = backoffDelay(failures++, { baseMs: FRAME_POLL_MS, maxMs: 8000, factor: 2, jitter: 0.2 });
      }
      if (seekTs === null && !ctl.signal.aborted) timer = setTimeout(() => void tick(), delay);
    };
    void tick();
    return () => {
      ctl.abort();
      clearTimeout(timer);
    };
  }, [api, streamId, seekTs, maxW]);

  useEffect(
    () => () => {
      if (current.current) URL.revokeObjectURL(current.current);
    },
    [],
  );
  return { url, tsMs, error };
}

export function useInterval(fn: () => void, ms: number) {
  const ref = useRef(fn);
  ref.current = fn;
  useEffect(() => {
    const t = setInterval(() => ref.current(), ms);
    return () => clearInterval(t);
  }, [ms]);
}
