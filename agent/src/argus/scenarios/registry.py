"""Scenario pack discovery, loading and validation (rules in PACK_FORMAT.md)."""

from __future__ import annotations

import importlib.util
import os
import re
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

import yaml

from argus.protocol import EVENT_TYPE_RE
from argus.scenarios.base import (
    CORE_TOOL_NAMES,
    EventTypeSpec,
    NullState,
    Sampling,
    ScenarioState,
    ScenarioTool,
)

PACK_NAME_RE = re.compile(r"^[a-z][a-z0-9_]*$")
ATTR_TYPES = {"string", "number", "boolean", "array", "object"}
SEVERITIES = {"low", "medium", "high"}
BUILTIN_DIR = Path(__file__).parent


class PackError(ValueError):
    """Raised when a pack violates PACK_FORMAT.md."""


@dataclass(slots=True)
class Pack:
    name: str
    version: int
    description: str
    actors: list[str]
    event_types: dict[str, EventTypeSpec]
    severity: dict[str, str]
    vision_system: str
    chat_system: str
    sampling: Sampling
    minute_summary_prompt: str
    state_cls: type[ScenarioState]
    tools: list[ScenarioTool]
    root: Path
    extra: dict[str, Any] = field(default_factory=dict)

    @property
    def fixtures_dir(self) -> Path:
        return self.root / "fixtures"

    def new_state(self) -> ScenarioState:
        return self.state_cls()

    def tool_names(self) -> list[str]:
        return [t.name for t in self.tools]


def _import_file(modname: str, path: Path) -> Any:
    spec = importlib.util.spec_from_file_location(modname, path)
    if spec is None or spec.loader is None:
        raise PackError(f"cannot import {path}")
    mod = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(mod)
    except Exception as e:  # surface pack bugs as pack errors
        raise PackError(f"error importing {path.name}: {e}") from e
    return mod


def _req_str(d: dict[str, Any], key: str, where: str) -> str:
    v = d.get(key)
    if not isinstance(v, str) or not v.strip():
        raise PackError(f"{where}: `{key}` must be a non-empty string")
    return v


def load_pack(path: str | Path) -> Pack:
    """Load and validate the pack in directory ``path`` (or a builtin pack by bare name)."""
    p = Path(path)
    if not p.exists() and (BUILTIN_DIR / str(path)).is_dir():
        p = BUILTIN_DIR / str(path)
    yml = p / "pack.yaml"
    if not yml.is_file():
        raise PackError(f"{p}: missing pack.yaml")
    try:
        raw = yaml.safe_load(yml.read_text())
    except yaml.YAMLError as e:
        raise PackError(f"{yml}: invalid YAML: {e}") from e
    if not isinstance(raw, dict):
        raise PackError(f"{yml}: top level must be a mapping")

    name = _req_str(raw, "name", "pack")
    if not PACK_NAME_RE.match(name):
        raise PackError(f"pack name {name!r} must match {PACK_NAME_RE.pattern}")
    if name != p.name:
        raise PackError(f"pack name {name!r} must equal directory name {p.name!r}")
    version = raw.get("version", 1)
    if not isinstance(version, int) or isinstance(version, bool) or version < 1:
        raise PackError("version must be a positive integer")
    actors = raw.get("actors") or []
    if not isinstance(actors, list) or not all(isinstance(a, str) for a in actors):
        raise PackError("actors must be a list of strings")

    types: dict[str, EventTypeSpec] = {}
    raw_types = raw.get("event_types")
    if not isinstance(raw_types, list) or not raw_types:
        raise PackError("event_types must be a non-empty list")
    for i, et in enumerate(raw_types):
        if not isinstance(et, dict):
            raise PackError(f"event_types[{i}] must be a mapping")
        t = et.get("type")
        if not isinstance(t, str) or not EVENT_TYPE_RE.match(t):
            raise PackError(f"event_types[{i}].type {t!r} must match {EVENT_TYPE_RE.pattern}")
        if t == "none":
            raise PackError("event type `none` is reserved")
        if t in types:
            raise PackError(f"duplicate event type {t!r}")
        attrs = et.get("attrs") or {}
        if not isinstance(attrs, dict):
            raise PackError(f"{t}: attrs must be a mapping")
        for k, v in attrs.items():
            if v not in ATTR_TYPES:
                raise PackError(f"{t}: attr {k!r} has type {v!r}; expected one of {sorted(ATTR_TYPES)}")
        types[t] = EventTypeSpec(
            t, str(et.get("description", "")), {str(k): str(v) for k, v in attrs.items()}
        )

    severity = raw.get("severity") or {}
    if not isinstance(severity, dict):
        raise PackError("severity must be a mapping")
    for t, s in severity.items():
        if t not in types:
            raise PackError(f"severity references unknown event type {t!r}")
        if s not in SEVERITIES:
            raise PackError(f"severity for {t!r} must be one of {sorted(SEVERITIES)}")

    prompts = raw.get("prompts") or {}
    vision_system = _req_str(prompts, "vision_system", "prompts")
    chat_system = _req_str(prompts, "chat_system", "prompts")

    s = raw.get("sampling") or {}
    try:
        sampling = Sampling(
            float(s.get("min_motion", 0.02)),
            int(s.get("max_interval_ms", 5000)),
            int(s.get("batch_frames", 3)),
            float(s.get("event_cooldown_s", 30.0)),
            {str(k): float(v) for k, v in (s.get("cooldowns") or {}).items()},
            int(s.get("max_events_per_minute", 60)),
        )
    except (TypeError, ValueError) as e:
        raise PackError(f"invalid sampling: {e}") from e
    if (
        not (0 <= sampling.min_motion <= 1)
        or sampling.max_interval_ms < 1
        or not (1 <= sampling.batch_frames <= 16)
    ):
        raise PackError("sampling out of range (min_motion 0..1, max_interval_ms>=1, batch_frames 1..16)")
    if (
        sampling.event_cooldown_s < 0
        or sampling.max_events_per_minute < 1
        or any(v < 0 or k not in types for k, v in sampling.cooldowns.items())
    ):
        raise PackError("sampling cooldowns must be >= 0 for declared types; max_events_per_minute >= 1")
    comp = raw.get("compaction") or {}
    minute_prompt = str(comp.get("minute_summary_prompt") or "Summarize these events in one sentence.")

    state_cls: type[ScenarioState] = NullState
    state_py = p / "state.py"
    if state_py.is_file():
        mod = _import_file(f"argus_pack_{name}_state", state_py)
        cls = getattr(mod, "State", None)
        if not (isinstance(cls, type) and issubclass(cls, ScenarioState)):
            raise PackError("state.py must define `State(ScenarioState)`")
        state_cls = cls

    tools: list[ScenarioTool] = []
    tools_py = p / "tools.py"
    if tools_py.is_file():
        mod = _import_file(f"argus_pack_{name}_tools", tools_py)
        tools = list(getattr(mod, "TOOLS", None) or [])
        seen: set[str] = set()
        for t in tools:
            if not isinstance(t, ScenarioTool):
                raise PackError("tools.py TOOLS must contain ScenarioTool instances")
            if t.name in CORE_TOOL_NAMES:
                raise PackError(f"tool {t.name!r} collides with a core tool")
            if t.name in seen:
                raise PackError(f"duplicate tool {t.name!r}")
            seen.add(t.name)

    return Pack(
        name=name,
        version=version,
        description=str(raw.get("description", "")),
        actors=list(actors),
        event_types=types,
        severity={str(k): str(v) for k, v in severity.items()},
        vision_system=vision_system,
        chat_system=chat_system,
        sampling=sampling,
        minute_summary_prompt=minute_prompt,
        state_cls=state_cls,
        tools=tools,
        root=p,
    )


class PackRegistry:
    """Discovers builtin packs plus ``extra_dirs`` / ``ARGUS_PACKS_DIR`` (each subdir with pack.yaml)."""

    def __init__(self, extra_dirs: list[str | Path] | None = None) -> None:
        dirs: list[Path] = [BUILTIN_DIR]
        env = os.environ.get("ARGUS_PACKS_DIR")
        for d in [*(extra_dirs or []), *([env] if env else [])]:
            dirs.append(Path(d))
        self._dirs = dirs
        self._packs: dict[str, Pack] = {}
        self.errors: dict[str, str] = {}
        self.reload()

    def reload(self) -> None:
        self._packs.clear()
        self.errors.clear()
        for d in self._dirs:
            if not d.is_dir():
                continue
            for sub in sorted(d.iterdir()):
                if sub.is_dir() and (sub / "pack.yaml").is_file():
                    try:
                        pack = load_pack(sub)
                    except PackError as e:
                        self.errors[sub.name] = str(e)
                        continue
                    self._packs[pack.name] = pack

    def names(self) -> list[str]:
        return sorted(self._packs)

    def get(self, name: str) -> Pack | None:
        return self._packs.get(name)

    def all(self) -> list[Pack]:
        return [self._packs[n] for n in self.names()]
