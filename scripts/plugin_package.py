"""Reproducible, platform-independent Agent Workflow plugin packaging."""
import json
from pathlib import Path
import re
import stat
import zipfile

ROOT = Path(__file__).resolve().parents[1]
PLUGIN = Path("plugins/agent-workflow")


def plugin_files(root=ROOT):
    return {path.relative_to(root).as_posix(): (0o755 if path.stat().st_mode & 0o111 else 0o644)
            for path in sorted((root / PLUGIN).rglob("*"))
            if path.is_file() and "__pycache__" not in path.parts and path.suffix != ".pyc"}


def validate_plugin(version, root=ROOT):
    plugin = root / PLUGIN
    portable = json.loads((plugin / "plugin.json").read_text())
    native = json.loads((plugin / ".codex-plugin/plugin.json").read_text())
    config = json.loads((plugin / "compatibility.json").read_text())
    if portable["version"] != version or native["version"] != version or config["engine_version"] != version:
        raise ValueError("plugin and engine release versions must match")
    if config["codex_versions"] != [json.loads((root / "images/pins.json").read_text())["codex_cli"]]:
        raise ValueError("plugin Codex compatibility must match the repository pin")
    if portable["$schema"] != "https://agent-plugins.org/schemas/1.0.0/plugin.schema.json" or native["skills"] != "./skills/":
        raise ValueError("invalid plugin manifest")
    if portable["name"] != "agent-workflow" or native["name"] != "agent-workflow":
        raise ValueError("invalid plugin name")
    for name in ("LICENSE", "NOTICE"):
        if (plugin / name).read_bytes() != (root / name).read_bytes():
            raise ValueError("plugin must preserve repository licensing: " + name)
    skills = {path.parent.name for path in (plugin / "skills").glob("*/SKILL.md")}
    if skills != {"setup", "build-workflow", "run-workflow", "report-bug"}:
        raise ValueError("plugin must bundle all four self-contained skills")
    for name in skills:
        path = plugin / "skills" / name / "SKILL.md"
        text = path.read_text()
        if not re.match(r"---\nname: " + name + r"\ndescription: [^\n]+\n---\n", text):
            raise ValueError("invalid skill metadata: " + name)
    for path in plugin.rglob("*.md"):
        for target in re.findall(r"\]\(([^)]+)\)", path.read_text()):
            if "://" not in target and not target.startswith("#"):
                resolved = (path.parent / target.split("#")[0]).resolve()
                if not resolved.is_relative_to(plugin.resolve()) or not resolved.is_file():
                    raise ValueError("missing or escaping plugin reference: " + target)
    for name in ("agent-workflow", "bug-report.py"):
        if stat.S_IMODE((plugin / "scripts" / name).stat().st_mode) != 0o755:
            raise ValueError("plugin launcher must be executable: " + name)


def build_plugin(output, version, root=ROOT):
    validate_plugin(version, root)
    path = Path(output) / ("agent-workflow-plugin-" + version + ".zip")
    # Fixed DOS epoch, sorted members, explicit Unix modes and no host metadata.
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as bundle:
        for name, mode in plugin_files(root).items():
            relative = Path(name).relative_to(PLUGIN)
            entry = zipfile.ZipInfo("agent-workflow/" + relative.as_posix(), (1980, 1, 1, 0, 0, 0))
            entry.create_system = 3
            entry.external_attr = (stat.S_IFREG | mode) << 16
            entry.compress_type = zipfile.ZIP_DEFLATED
            bundle.writestr(entry, (root / name).read_bytes(), compresslevel=9)
    return path


def verify_plugin(path, version, root=ROOT):
    validate_plugin(version, root)
    expected = {"agent-workflow/" + Path(name).relative_to(PLUGIN).as_posix(): (name, mode)
                for name, mode in plugin_files(root).items()}
    with zipfile.ZipFile(path) as bundle:
        if len(bundle.namelist()) != len(expected) or set(bundle.namelist()) != set(expected):
            raise ValueError("plugin ZIP asset set mismatch")
        for entry in bundle.infolist():
            name, mode = expected[entry.filename]
            if entry.external_attr >> 16 != stat.S_IFREG | mode or bundle.read(entry) != (root / name).read_bytes():
                raise ValueError("plugin ZIP content/mode mismatch: " + name)
