# Argus

Real-time video understanding you can talk to. Argus watches one or more live video
streams, keeps a rolling one-hour history of what happened, and lets you ask about it by
voice or text — "what is the dog doing?", "did anyone come to the door in the last ten
minutes?", or, with the pickleball pack, "what's the score?".

Each use case is a **scenario pack**; two ship as examples (`home_monitor` and
`pickleball_referee`). Models sit behind provider adapters, and the repository ships
deterministic fakes for all of them, so everything builds and tests offline. Real providers
(Anthropic, OpenAI, Ollama, local models) are configured in `agent/providers.yaml`.

| Component | Language | Role |
|---|---|---|
| [`mediacore/`](mediacore) | C++20 | frame store with 1 h retention, capture, image ops, audio ring/VAD/jitter — C ABI |
| [`gateway/`](gateway) | Go | streams, auth/quotas, duplex WebSocket sessions, clip export |
| [`agent/`](agent) | Python 3.13 | perception, memory + RAG (SQLite + sqlite-vec), scenario packs, conversation |
| [`console/`](console) | TypeScript | web client: stream grid, mic/playback with barge-in, 1 h timeline |

See [`CONTRACTS.md`](CONTRACTS.md) for the interfaces between components, and each
component's README for build and test instructions. CI builds and tests all four
components.

> This repository is used as the host codebase for an automated code-review benchmark; some
> pull requests are opened programmatically.

License: MIT.
