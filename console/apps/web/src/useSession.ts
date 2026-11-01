import { useCallback, useEffect, useMemo, useReducer, useRef, useState } from "react";
import type { ApiClient } from "./lib/api";
import { SessionClock } from "./lib/clock";
import { AudioPlayer } from "./lib/audio/player";
import { Mic } from "./lib/audio/mic";
import { initialSessionState, sessionReducer } from "./lib/sessionState";
import { connectionManager } from "./lib/connection";
import type { SocketStatus } from "./lib/ws";
import { WINDOW_MS } from "./lib/timeline";

export function useSession(api: ApiClient) {
  const [state, dispatch] = useReducer(sessionReducer, initialSessionState);
  const [status, setStatus] = useState<SocketStatus>("idle");
  const [error, setError] = useState<string | null>(null);
  const [talking, setTalking] = useState(false);
  const clock = useMemo(() => new SessionClock(), []);
  const unsubs = useRef<Array<() => void>>([]);
  const sessionId = useRef<string | null>(null);
  const player = useRef<AudioPlayer | null>(null);
  const mic = useRef<Mic | null>(null);
  const seq = useRef(0);
  const talkingRef = useRef(false);
  const reqN = useRef(0);

  const stop = useCallback(async () => {
    connectionManager.send({ type: "control.stop" });
    unsubs.current.forEach((off) => off());
    unsubs.current = [];
    connectionManager.disconnect();
    await mic.current?.stop();
    await player.current?.dispose();
    player.current = null;
    clock.stop();
    const id = sessionId.current;
    sessionId.current = null;
    if (id) await api.deleteSession(id).catch(() => undefined);
    setTalking(false);
  }, [api, clock]);

  const start = useCallback(
    async (scenario: string, streamIds: string[]) => {
      setError(null);
      await stop();
      dispatch({ type: "reset" });
      try {
        const info = await api.createSession({ scenario, stream_ids: streamIds });
        sessionId.current = info.id;
        player.current = new AudioPlayer();
        mic.current = new Mic({
          onChunk: (pcm16_b64) => {
            connectionManager.send({
              type: "audio.chunk",
              seq: seq.current++,
              ts_ms: clock.ms(),
              sample_rate: 16000,
              pcm16_b64,
            });
          },
        });
        unsubs.current.push(connectionManager.on("status", (i) => setStatus(i.status)));
        unsubs.current.push(
          connectionManager.on("message", (msg) => {
            dispatch({ type: "server", msg });
            switch (msg.type) {
              case "session.started": {
                clock.start();
                const wallNow = Date.now();
                connectionManager.send({
                  type: "history.query",
                  request_id: `seed-${++reqN.current}`,
                  since_ms: wallNow - WINDOW_MS, // wall-clock epoch ms (CONTRACTS clarification)
                  until_ms: wallNow,
                  limit: 200,
                });
                break;
              }
              case "assistant.audio":
                player.current?.enqueue(msg);
                break;
              case "assistant.cancelled":
                player.current?.cancel(msg.utterance_id);
                break;
              case "transcript.partial":
                // Auto barge-in: the user started talking while the assistant was audible.
                if (player.current?.isPlaying) player.current.flush();
                break;
            }
          }),
        );
        connectionManager.connect(() => api.wsUrl(info.id), scenario, streamIds);
      } catch (e) {
        setError(e instanceof Error ? e.message : String(e));
        sessionId.current = null;
      }
    },
    [api, clock, stop],
  );

  useEffect(() => () => void stop(), [stop]);

  const sendText = useCallback((text: string) => {
    const t = text.trim();
    if (!t) return;
    if (connectionManager.send({ type: "text.message", text: t })) {
      dispatch({ type: "local.user_text", text: t, key: `local:${Date.now()}:${Math.random()}` });
    }
  }, []);
  const bargeIn = useCallback(() => {
    player.current?.flush();
    connectionManager.send({ type: "control.barge_in" });
  }, []);
  const pttStart = useCallback(async () => {
    talkingRef.current = true;
    setTalking(true);
    try {
      await mic.current?.start();
      if (!talkingRef.current) await mic.current?.stop(); // released while permission prompt was open
    } catch (e) {
      talkingRef.current = false;
      setTalking(false);
      setError(`Microphone: ${e instanceof Error ? e.message : String(e)}`);
    }
  }, []);
  const pttStop = useCallback(async () => {
    talkingRef.current = false;
    setTalking(false);
    await mic.current?.stop();
  }, []);

  return { state, status, error, talking, clock, start, stop, sendText, bargeIn, pttStart, pttStop };
}
