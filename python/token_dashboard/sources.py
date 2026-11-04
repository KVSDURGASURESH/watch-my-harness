"""Which agents' session logs get scanned.

Claude Code (~/.claude/projects) is always a source. Others come from, in
order: a JSON config file, then repeatable ``--source name=kind:path`` flags.
Kinds: ``claude``, ``codex``, ``hermes``, ``generic`` (see scanner.KINDS).
A config entry may add ``"model_alias": {"ojas-qwen": "qwen3.5"}`` (hermes) to rename gateway aliases.
"""
from __future__ import annotations

import json
import os
import re
import threading
from pathlib import Path
from typing import List, Optional

from .scanner import KINDS

CODEX_DEFAULT = "~/.codex/sessions"


def default_config_path() -> Path:
    return Path(os.environ.get("TOKEN_DASHBOARD_SOURCES") or Path.home() / ".claude" / "token-dashboard-sources.json")


def parse_source_arg(spec: str) -> dict:
    """``name=kind:path`` (or ``name=path`` when name is itself a kind)."""
    name, sep, rest = spec.partition("=")
    if not sep or not name or not rest:
        raise ValueError(f"bad --source {spec!r}; expected name=kind:path, e.g. codex=codex:~/.codex/sessions")
    kind, sep, path = rest.partition(":")
    # "kind:path" vs a bare path (which may itself contain ':' on Windows)
    if not sep or kind not in KINDS:
        kind, path = (name, rest)
    if kind not in KINDS:
        raise ValueError(f"unknown kind {kind!r} in --source {spec!r}; expected one of {sorted(KINDS)}")
    return {"name": name, "kind": kind, "path": path}


def load_entries(path: Optional[Path] = None) -> List[dict]:
    """The editable rows of the sources file, validated shallowly and otherwise untouched."""
    path = Path(path) if path else default_config_path()
    if not path.is_file():
        return []
    rows = json.loads(path.read_text(encoding="utf-8"))
    for e in rows:
        if e.get("kind") not in KINDS or not e.get("name") or not e.get("path"):
            raise ValueError(f"{path}: each entry needs name, kind ({sorted(KINDS)}), path; got {e!r}")
    return rows


def load_config(path: Optional[Path] = None) -> List[dict]:
    path = Path(path) if path else default_config_path()
    if not path.is_file():
        return []
    out = []
    for e in load_entries(path):
        if e.get("enabled") is False:
            continue
        out.append({"name": e["name"], "kind": e["kind"], "path": e["path"],
                    **({"options": {"model_alias": e["model_alias"]}} if e.get("model_alias") else {})})
    return out


def resolve_sources(claude_dir: str, use_config: bool = True, extra: Optional[List[str]] = None,
                    config_path: Optional[Path] = None) -> List[dict]:
    """Claude Code first, then config-file sources, then --source flags.

    A later entry with the same name replaces an earlier one. Callers pass
    use_config=False when the user pinned --projects-dir, so an explicit
    directory means exactly that directory.
    """
    by_name = {"claude": {"name": "claude", "kind": "claude", "path": claude_dir}}
    if use_config:
        for s in load_config(config_path):
            by_name[s["name"]] = s
    for spec in extra or []:
        s = parse_source_arg(spec)
        by_name[s["name"]] = s
    return list(by_name.values())


# ---------------------------------------------------------------- editing (Settings > Sources, setup)

_NAME_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,39}$")


def validate(entries) -> None:
    if not isinstance(entries, list):
        raise ValueError("sources must be a list")
    seen = set()
    for e in entries:
        if not isinstance(e, dict):
            raise ValueError("each source must be an object")
        name = e.get("name") or ""
        if not _NAME_RE.match(name):
            raise ValueError(f"bad source name {name!r} (letters, digits, _ . - ; max 40)")
        if e.get("kind") not in KINDS:
            raise ValueError(f"source {name!r}: kind must be one of {sorted(KINDS)}")
        if not str(e.get("path") or "").strip():
            raise ValueError(f"source {name!r}: path is required")
        if name in seen:
            raise ValueError(f"duplicate source name {name!r}")
        seen.add(name)


def save_entries(path: Path, entries: List[dict]) -> None:
    validate(entries)
    keep = ("name", "kind", "path", "model_alias", "enabled")
    rows = [{k: e[k] for k in keep if k in e and e[k] is not None} for e in entries]
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp")
    tmp.write_text(json.dumps(rows, indent=2) + "\n", encoding="utf-8")
    os.replace(tmp, path)


def _count_files(kind: str, root: Path) -> int:
    root = root.expanduser()
    if kind == "hermes":
        n = int((root / "state.db").is_file())
        prof = root / "profiles"
        if prof.is_dir():
            n += sum(1 for d in prof.iterdir() if (d / "state.db").is_file())
        return n
    if not root.is_dir():
        return 0
    return sum(1 for _ in root.rglob("*.jsonl"))


def detect(claude_dir: str) -> List[dict]:
    """The well-known harness folders, for the first-run setup screen."""
    cands = [("claude", "claude", claude_dir), ("codex", "codex", CODEX_DEFAULT),
             ("hermes", "hermes", os.environ.get("HERMES_HOME") or "~/.hermes")]
    out = []
    for name, kind, path in cands:
        files = _count_files(kind, Path(path))
        out.append({"name": name, "kind": kind, "path": path, "exists": files > 0, "files": files})
    return out


class Manager:
    """Resolves the effective source list on demand, so edits made in the UI apply on the next scan."""

    def __init__(self, claude_dir: str, pinned: bool = False, extra: Optional[List[str]] = None,
                 config_path: Optional[Path] = None):
        self.claude_dir = claude_dir
        self.pinned = pinned
        self.extra = list(extra or [])
        self.config_path = Path(config_path) if config_path else default_config_path()
        self._lock = threading.Lock()

    def entries(self) -> List[dict]:
        with self._lock:
            return load_entries(self.config_path)

    def set_entries(self, entries: List[dict]) -> None:
        with self._lock:
            save_entries(self.config_path, entries)

    def effective(self) -> List[dict]:
        return resolve_sources(self.claude_dir, use_config=not self.pinned, extra=self.extra,
                               config_path=self.config_path)
