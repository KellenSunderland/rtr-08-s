import textwrap

import pytest

from argus.scenarios.registry import PackError, PackRegistry, load_pack

GOOD = """
name: {name}
event_types:
  - type: a.b
    description: d
prompts: {{vision_system: v, chat_system: c}}
"""


def write(tmp_path, name, body, dirname=None):
    d = tmp_path / (dirname or name)
    d.mkdir()
    (d / "pack.yaml").write_text(textwrap.dedent(body))
    return d


def test_builtin_packs_load(packs):
    assert packs.names() == ["home_monitor", "pickleball_referee"]
    assert not packs.errors
    pb = packs.get("pickleball_referee")
    assert {
        "rally.start",
        "ball.bounce",
        "fault.kitchen",
        "fault.serve",
        "point.won",
        "state.snapshot",
    } <= set(pb.event_types)
    assert pb.tool_names() == ["current_score", "last_point"]
    assert packs.get("home_monitor").tool_names() == ["last_seen", "activity"]
    assert pb.new_state().snapshot()["score"] == {"A": 0, "B": 0}


def test_valid_custom(tmp_path):
    assert load_pack(write(tmp_path, "mine", GOOD.format(name="mine"))).name == "mine"


@pytest.mark.parametrize(
    ("mut", "msg"),
    [
        (lambda s: s.replace("name: x", "name: Bad-Name"), "must match"),
        (lambda s: s.replace("type: a.b", "type: A.B"), "must match"),
        (
            lambda s: s.replace(
                "- type: a.b\n    description: d", "- type: a.b\n    description: d\n  - type: a.b"
            ),
            "duplicate",
        ),
        (lambda s: s.replace("a.b", "none"), "reserved"),
        (lambda s: s + "severity: {zzz.q: high}\n", "unknown event type"),
        (lambda s: s + "severity: {a.b: urgent}\n", "one of"),
        (lambda s: s.replace("chat_system: c", "chat_system: ''"), "chat_system"),
        (lambda s: s + "sampling: {batch_frames: 0}\n", "sampling"),
        (lambda s: s.replace("description: d", "description: d\n    attrs: {k: banana}"), "attr"),
    ],
)
def test_validation_errors(tmp_path, mut, msg):
    d = write(tmp_path, "x", mut(GOOD.format(name="x")))
    with pytest.raises(PackError, match=msg):
        load_pack(d)


def test_name_must_equal_dir(tmp_path):
    with pytest.raises(PackError, match="directory name"):
        load_pack(write(tmp_path, "other", GOOD.format(name="other"), dirname="dirname"))


def test_missing_yaml_and_bad_yaml(tmp_path):
    (tmp_path / "e").mkdir()
    with pytest.raises(PackError, match="missing"):
        load_pack(tmp_path / "e")
    d = write(tmp_path, "y", "name: [unclosed")
    with pytest.raises(PackError, match="YAML"):
        load_pack(d)


def test_tool_collision_and_bad_state(tmp_path):
    d = write(tmp_path, "t", GOOD.format(name="t"))
    (d / "tools.py").write_text(
        "from argus.scenarios.base import ScenarioTool\n"
        "async def h(ctx): return 'x'\n"
        "TOOLS=[ScenarioTool('search_history','d',{},h)]\n"
    )
    with pytest.raises(PackError, match="collides"):
        load_pack(d)
    (d / "tools.py").unlink()
    (d / "state.py").write_text("class State: pass\n")
    with pytest.raises(PackError, match="State"):
        load_pack(d)


def test_registry_reports_invalid(tmp_path):
    write(tmp_path, "good", GOOD.format(name="good"))
    write(tmp_path, "bad", GOOD.format(name="nope"))
    reg = PackRegistry([tmp_path])
    assert "good" in reg.names() and "bad" in reg.errors
