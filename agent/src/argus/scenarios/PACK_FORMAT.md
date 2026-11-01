# Scenario packs

A scenario pack adapts the use-case-agnostic core to one situation. Packs live in
`argus/scenarios/<name>/` and are discovered by the registry at startup.

```
argus/scenarios/<name>/
  pack.yaml          # required: metadata, taxonomy, prompts
  state.py           # optional: class State(ScenarioState) — reducer over events
  tools.py           # optional: TOOLS: list[ScenarioTool] — extra agent tools
  fixtures/          # optional: fake-provider scripts for tests/demos
```

## pack.yaml

```yaml
name: home_monitor
version: 1
description: Watches rooms/doors; answers "what is the dog doing?", "did anyone come in?"
actors: [person, dog, cat, package]           # labels the Detector/VisionModel may emit
event_types:                                  # the taxonomy; `type` must match protocol SceneEvent.type
  - type: dog.eating
    description: A dog is eating from a bowl.
    attrs: { bowl_location: string }          # JSON-schema-ish attr hints (string|number|boolean|array)
  - type: dog.sleeping
    description: A dog is lying down with eyes closed / still for >30s.
  - type: person.at_door
    description: A person is at an entry door (inside or outside).
    attrs: { known: boolean }
  - type: person.entered
    description: A person crossed from outside to inside.
severity:                                     # optional: used by the compactor to keep important events in summaries
  person.entered: high
prompts:
  vision_system: |                            # given to VisionModel.observe
    You watch a home camera. Report only events from the list...
  chat_system: |                              # prepended to the ChatModel system prompt
    You are Argus, a calm home assistant...
sampling:
  min_motion: 0.02                            # forwarded to gateway /internal/frames
  max_interval_ms: 5000
  batch_frames: 3                             # frames per VisionModel.observe call
  event_cooldown_s: 30                        # same-type events within this window collapse into the first (0 = off)
  cooldowns: { dog.sleeping: 120 }            # per-type override of event_cooldown_s
  max_events_per_minute: 60                   # per-stream cap; lowest-confidence events dropped first
compaction:
  minute_summary_prompt: |
    Summarize these events in one sentence...
```

## state.py

```python
from argus.scenarios.base import ScenarioState


class State(ScenarioState):
    """Pure reducer: apply(event) -> None; snapshot() -> dict. Must be deterministic."""
```

The pickleball pack keeps score/serve/side; the core calls `apply` for every
accepted event in `ts_ms` order (per session) and exposes `snapshot()` to tools
and to the console scoreboard.

## tools.py

```python
from argus.scenarios.base import ScenarioTool

TOOLS = [ScenarioTool(name="current_score", description=..., input_schema={...}, handler=async_fn)]
```

Handlers receive `(ctx: ToolContext, **args)`; `ctx` gives access to memory
search, the state snapshot, and the session clock. Tool names must not collide
with core tools: `search_history`, `what_happened`, `describe_now`.

## Validation rules (enforced by `argus.scenarios.registry.load_pack`)

- `name` matches `^[a-z][a-z0-9_]*$` and equals the directory name.
- `event_types[].type` unique, matches `^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$`.
- A VisionModel observation whose `event_type` is not in the taxonomy (and not `none`) is dropped and counted in metrics.
- Collapsed duplicates update the first event's `attrs.repeat_count` / `attrs.last_seen_wall_ms`; `state.snapshot`
  and `severity: high` types are never collapsed or capped. `cooldowns` keys must be declared types.
- `attrs` returned by the model are filtered to declared keys.
