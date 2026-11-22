# Argus — component contracts

Argus is a real-time video-understanding assistant you can talk to. Four components,
one monorepo. This file is the integration contract; the files it points at are the
source of truth. Builders of each component must implement exactly these interfaces
and must update this file in the same change if they alter one.

```
console (TS, browser)  ──WS/REST──▶  gateway (Go)  ──cgo──▶  mediacore (C++)
                                        │  ▲
                            internal WS │  │ events/summaries (HTTP)
                                        ▼  │
                                      agent (Python)  ──ctypes──▶ mediacore (VAD/resample only)
```

| Contract | File | Mirrored by |
|---|---|---|
| mediacore C ABI | `mediacore/include/mediacore_c.h` | gateway (cgo), agent (ctypes) |
| Wire protocol (WS + REST types) | `console/packages/protocol/src/index.ts` (zod) | gateway `internal/protocol` (Go structs), agent `argus/protocol.py` (pydantic) |
| Gateway REST/WS | `gateway/api/openapi.yaml` | console client, agent internal server |
| Provider protocols | `agent/src/argus/providers/base.py` | — |
| Scenario pack format | `agent/src/argus/scenarios/PACK_FORMAT.md` | — |
| Memory schema | `agent/src/argus/memory/schema.sql` | — |

## Responsibilities

**mediacore (C++20)** — pure library, no network. Frame store with 1h retention and
thinning, capture via OpenCV, image ops, audio ring/resample/mix, VAD, jitter buffer.
Exposes only the C ABI. Must be buildable with `cmake -S mediacore -B build && cmake --build build && ctest --test-dir build`.
Produces `libmediacore` (shared + static) and installs the header.

**gateway (Go 1.23)** — owns streams: opens captures (one goroutine per stream),
pushes frames into ONE shared `mc_frame_store`, runs `mc_frame_store_prune` every
second, tracks per-stream state/fps. Serves REST + the console WS. For each session,
dials the agent's `/internal/sessions/{id}/ws` and relays. Serves
`/internal/frames/{stream_id}` to the agent using `mc_motion_score` gating. Clip
export zips frames from the store. Auth tokens + quotas from `gateway.yaml`.
Session clock: `ts_ms = (mono_now - session_start_mono) / 1e6`; the gateway stamps
every frame and audio chunk it forwards. No model calls, no SQLite.

**agent (Python 3.13)** — perception: one worker per (session, stream) consuming
`/internal/frames`, batching frames per pack `sampling.batch_frames`, calling
`VisionModel.observe`, validating against the pack taxonomy, writing `SceneEvent`s
to memory and pushing them to the session WS as `event`. Memory: SQLite + sqlite-vec,
hybrid search (time window filter + vector top-k, re-ranked by recency), minute
compaction, retention job. Conversation: ASR → ChatModel (tools: `search_history`,
`what_happened`, `describe_now`, + pack tools) → TTS, with barge-in cancelling the
in-flight utterance. Serves the internal WS (gateway is the client) and
`GET /v1/scenarios`, `GET /v1/history` (the gateway proxies these). Uses mediacore
via ctypes only for VAD and resampling.

**console (TS)** — Vite + React app: register streams, start a session, mic capture
(16 kHz PCM16 via AudioWorklet) → `audio.chunk`, playback of `assistant.audio` with
barge-in (local VAD-less: push-to-talk + "interrupt" button + auto barge-in when the
server sends `transcript.partial` during playback), stream grid with `/v1/streams/{id}/frame`
polling, 1h timeline scrubber built from `event`/`summary` messages, transcript panel,
scenario-specific panel (scoreboard for pickleball). `packages/protocol` is published
to the workspace and must have 100% schema coverage tests.

## Cross-cutting rules

- **Clocks**: use the monotonic clock for durations and the wall clock for retention and
  display. Each component keeps both; never derive one from the other after the fact (a wall
  timestamp minus a monotonic one is meaningless).
- **Backpressure**: frame paths drop frames rather than queue without bound; audio paths use
  the jitter buffer or ring, with drop counters surfaced in `stream.status` and metrics.
- **Security baseline** (builders must implement; the benchmark will later plant
  violations): stream URIs validated against scheme/host allow-list; file URIs
  confined to media root (no `..`); clip export paths confined; transcripts never
  logged at INFO; internal endpoints bound to localhost + `X-Internal-Token`;
  bearer tokens compared in constant time; WS messages size-limited (64 KiB text,
  256 KiB binary); SQL via parameters only; on-screen text from frames is treated
  as data (the vision prompt instructs the model to never follow instructions seen
  in images, and the agent strips `instruction-like` text from `summary` before it
  reaches the ChatModel context — see `argus/perception/sanitize.py`).
- **Observability**: structured logs (JSON), `/metrics` Prometheus text on gateway
  and agent.
- **Tests**: every component has unit tests runnable offline with fakes. CI builds all
  four on ubuntu-latest (`.github/workflows/ci.yml`).

## Clarifications (added after the console build; binding)

- **WS auth**: browsers cannot set `Authorization` on a WebSocket handshake. All WS endpoints
  (`/v1/sessions/{id}/ws`, and the internal ones) accept the bearer token either in the
  `Authorization` header or as `?token=<token>` query parameter. Internal endpoints accept
  `X-Internal-Token` header or `?token=`.
- **History time domain**: `history.query.since_ms/until_ms` and `GET /v1/history?since_ms&until_ms`
  are **wall-clock Unix epoch milliseconds** (the history spans sessions; the field names are
  kept for compatibility). Returned `SceneEvent.ts_ms` remains the originating session's clock;
  consumers display by `wall_ms`. The console seeds the timeline with `[now_wall − 1h, now_wall]`.
- **`GET /v1/history` response** is `{ events, summaries, truncated }` (no `type`/`request_id`).
- **pickleball_referee attrs**: `point.won` carries `attrs.team ∈ {"A","B"}` and `attrs.reason`;
  `ball.bounce` carries `attrs.court_side ∈ {"A","B"}`, `attrs.in_bounds: boolean`;
  `fault.*` carries `attrs.team`. The pack also emits a `state.snapshot` event
  (`type: "state.snapshot"`, `attrs` = `State.snapshot()`) after every scoring change so the
  console scoreboard can render without re-deriving.
- **Frames**: `/v1/streams/{id}/frame` is `image/jpeg`; clients fetch with auth and render a blob URL.

## Identifiers

- `stream_id`: `^[a-z0-9][a-z0-9_-]*$`, ≤64 chars, unique per gateway.
- `session_id`: UUIDv4 generated by the gateway.
- `utterance_id`: `u-<counter>` per session, assigned by the agent.
- Event `type`: dotted lowercase from the pack taxonomy.
