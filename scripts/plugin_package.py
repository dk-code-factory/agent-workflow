"""Reproducible, platform-independent Agent Workflow plugin packaging."""
import json
from pathlib import Path, PurePosixPath
import re
import stat
import zipfile

ROOT = Path(__file__).resolve().parents[1]
PLUGIN = Path("plugins/agent-workflow")
# Deliberate built-in inventory. Stage 6 adds convert-agent here with its source
# and acceptance checks; third-party packages do not inherit this profile.
BUILTIN_SKILLS = frozenset({"setup", "build-workflow", "run-workflow", "report-bug"})


def plugin_files(root=ROOT):
    return {path.relative_to(root).as_posix(): (0o755 if path.stat().st_mode & 0o111 else 0o644)
            for path in sorted((root / PLUGIN).rglob("*"))
            if path.is_file() and "__pycache__" not in path.parts and path.suffix != ".pyc"}


def validate_builtin_profile(version, root=ROOT):
    """Release-only identity, licensing, compatibility and inventory policy."""
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
    if skills != BUILTIN_SKILLS:
        raise ValueError("plugin must bundle all four self-contained skills")
    return skills


def validate_plugin(version, root=ROOT):
    skills = validate_builtin_profile(version, root)
    validate_skill_files(root / PLUGIN, skills)
    for name in ("agent-workflow", "bug-report.py"):
        if stat.S_IMODE((root / PLUGIN / "scripts" / name).stat().st_mode) != 0o755:
            raise ValueError("plugin launcher must be executable: " + name)


def validate_skill_files(plugin, skills):
    """Structural checks independent of built-in name/version/licensing policy."""
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


def write_archive(path, files):
    """Write an explicit name -> (bytes, normalized mode) table reproducibly."""
    validate_archive_files(files)
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as bundle:
        for name in sorted(files):
            data, mode = files[name]
            entry = zipfile.ZipInfo(name, (1980, 1, 1, 0, 0, 0))
            entry.create_system = 3
            entry.external_attr = (stat.S_IFREG | mode) << 16
            entry.compress_type = zipfile.ZIP_DEFLATED
            bundle.writestr(entry, data, compresslevel=9)


def verify_archive(path, files):
    """Verify the complete explicit asset table, bytes and portable metadata."""
    validate_archive_files(files)
    with zipfile.ZipFile(path) as bundle:
        if bundle.namelist() != sorted(files):
            raise ValueError("plugin ZIP asset set mismatch")
        for entry in bundle.infolist():
            data, mode = files[entry.filename]
            if (entry.external_attr >> 16 != stat.S_IFREG | mode or bundle.read(entry) != data
                    or entry.date_time != (1980, 1, 1, 0, 0, 0) or entry.create_system != 3):
                raise ValueError("plugin ZIP content/mode mismatch: " + entry.filename)


def validate_archive_files(files):
    for name, (_, mode) in files.items():
        path = PurePosixPath(name)
        if (not name or path.is_absolute() or str(path) != name or ".." in path.parts
                or any(c in name for c in ("\\", "\0", "\r", "\n")) or mode not in (0o644, 0o755)):
            raise ValueError("unsafe plugin ZIP path/mode: " + name)
        if any(str(parent) in files for parent in path.parents if str(parent) != "."):
            raise ValueError("plugin ZIP file/directory collision: " + name)


def builtin_archive_files(root=ROOT):
    return {"agent-workflow/" + Path(name).relative_to(PLUGIN).as_posix(): ((root / name).read_bytes(), mode)
            for name, mode in plugin_files(root).items()}


def build_plugin(output, version, root=ROOT):
    validate_plugin(version, root)
    path = Path(output) / ("agent-workflow-plugin-" + version + ".zip")
    write_archive(path, builtin_archive_files(root))
    return path


def verify_plugin(path, version, root=ROOT):
    validate_plugin(version, root)
    verify_archive(path, builtin_archive_files(root))
