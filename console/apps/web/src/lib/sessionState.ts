import type { MinuteSummary, SceneEvent, ServerMessage, StreamInfo } from "@argus/protocol";
import { mergeEventUpdate } from "./merge";
import { mergeEvents, mergeSummaries } from "./timeline";

export interface TranscriptEntry {
  key: string;
  role: "user" | "assistant";
  text: string;
  partial: boolean;
  utteranceId?: string;
  cancelled?: boolean;
}

export interface SessionState {
  sessionId?: string;
  scenario?: string;
  streams: StreamInfo[];
  transcript: TranscriptEntry[];
  events: SceneEvent[];
  summaries: MinuteSummary[];
  errors: string[];
}

export const initialSessionState: SessionState = {
  streams: [],
  transcript: [],
  events: [],
  summaries: [],
  errors: [],
};

export type SessionAction =
  | { type: "server"; msg: ServerMessage }
  | { type: "local.user_text"; text: string; key: string }
  | { type: "reset" };

const MAX_TRANSCRIPT = 500;
const MAX_ERRORS = 20;

function trim<T>(a: T[], n: number): T[] {
  return a.length > n ? a.slice(a.length - n) : a;
}

export function sessionReducer(s: SessionState, a: SessionAction): SessionState {
  if (a.type === "reset") return initialSessionState;
  if (a.type === "local.user_text") {
    return {
      ...s,
      transcript: trim(
        [...s.transcript, { key: a.key, role: "user", text: a.text, partial: false }],
        MAX_TRANSCRIPT,
      ),
    };
  }
  const m = a.msg;
  switch (m.type) {
    case "session.started":
      // Keep history across reconnects; replace identity + stream list.
      return { ...s, sessionId: m.session_id, scenario: m.scenario, streams: m.streams };
    case "transcript.partial": {
      const last = s.transcript[s.transcript.length - 1];
      if (last && last.role === "user" && last.partial) {
        return { ...s, transcript: [...s.transcript.slice(0, -1), { ...last, text: m.text }] };
      }
      return {
        ...s,
        transcript: trim(
          [
            ...s.transcript,
            { key: `up:${s.transcript.length}:${m.ts_ms}`, role: "user", text: m.text, partial: true },
          ],
          MAX_TRANSCRIPT,
        ),
      };
    }
    case "transcript.final": {
      const last = s.transcript[s.transcript.length - 1];
      const entry: TranscriptEntry = {
        key: `u:${m.utterance_id}`,
        role: "user",
        text: m.text,
        partial: false,
        utteranceId: m.utterance_id,
      };
      if (last && last.role === "user" && last.partial)
        return { ...s, transcript: [...s.transcript.slice(0, -1), entry] };
      return { ...s, transcript: trim([...s.transcript, entry], MAX_TRANSCRIPT) };
    }
    case "assistant.text": {
      const key = `a:${m.utterance_id}`;
      const i = s.transcript.findIndex((t) => t.key === key);
      if (i < 0) {
        return {
          ...s,
          transcript: trim(
            [
              ...s.transcript,
              { key, role: "assistant", text: m.delta, partial: !m.final, utteranceId: m.utterance_id },
            ],
            MAX_TRANSCRIPT,
          ),
        };
      }
      const cur = s.transcript[i]!;
      if (cur.cancelled) return s; // late text after cancellation
      const next = s.transcript.slice();
      next[i] = { ...cur, text: cur.text + m.delta, partial: !m.final };
      return { ...s, transcript: next };
    }
    case "assistant.cancelled": {
      const key = `a:${m.utterance_id}`;
      const i = s.transcript.findIndex((t) => t.key === key);
      if (i < 0) return s;
      const next = s.transcript.slice();
      next[i] = { ...next[i]!, partial: false, cancelled: true };
      return { ...s, transcript: next };
    }
    case "event": {
      const prev = s.events.find((e) => e.id === m.event.id);
      return { ...s, events: mergeEvents(s.events, [prev ? mergeEventUpdate(prev, m.event) : m.event]) };
    }
    case "summary":
      return { ...s, summaries: mergeSummaries(s.summaries, [m.summary]) };
    case "history.result":
      return {
        ...s,
        events: mergeEvents(s.events, m.events),
        summaries: mergeSummaries(s.summaries, m.summaries),
      };
    case "stream.status": {
      const has = s.streams.some((x) => x.id === m.stream.id);
      return {
        ...s,
        streams: has ? s.streams.map((x) => (x.id === m.stream.id ? m.stream : x)) : [...s.streams, m.stream],
      };
    }
    case "error":
      return { ...s, errors: trim([...s.errors, `${m.code}: ${m.message}`], MAX_ERRORS) };
    case "assistant.audio":
      return s;
  }
}
