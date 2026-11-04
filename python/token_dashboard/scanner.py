"""JSONL transcript walker + parser."""
from __future__ import annotations

import hashlib
import json
import re
import sqlite3
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import List, Optional, Tuple, Union

from .db import connect, _encode_slug


INSERT_MSG = """
INSERT OR REPLACE INTO messages (
  uuid, parent_uuid, session_id, project_slug, cwd, git_branch, cc_version, entrypoint,
  type, is_sidechain, agent_id, timestamp, model, stop_reason, prompt_id, message_id,
  input_tokens, output_tokens, cache_read_tokens, cache_create_5m_tokens, cache_create_1h_tokens,
  prompt_text, prompt_chars, tool_calls_json, source
) VALUES (
  :uuid, :parent_uuid, :session_id, :project_slug, :cwd, :git_branch, :cc_version, :entrypoint,
  :type, :is_sidechain, :agent_id, :timestamp, :model, :stop_reason, :prompt_id, :message_id,
  :input_tokens, :output_tokens, :cache_read_tokens, :cache_create_5m_tokens, :cache_create_1h_tokens,
  :prompt_text, :prompt_chars, :tool_calls_json, :source
)
"""

INSERT_TOOL = """
INSERT INTO tool_calls (message_uuid, session_id, project_slug, tool_name, target, result_tokens, is_error, timestamp, source)
VALUES (:message_uuid, :session_id, :project_slug, :tool_name, :target, :result_tokens, :is_error, :timestamp, :source)
"""


_TARGET_FIELDS = {
    "Read":      "file_path",
    "Edit":      "file_path",
    "Write":     "file_path",
    "Glob":      "pattern",
    "Grep":      "pattern",
    "Bash":      "command",
    "WebFetch":  "url",
    "WebSearch": "query",
    "Task":      "subagent_type",
    "Skill":     "skill",
}


def _usage(rec: dict) -> dict:
    u = (rec.get("message") or {}).get("usage") or {}
    cc = u.get("cache_creation") or {}
    return {
        "input_tokens":           int(u.get("input_tokens") or 0),
        "output_tokens":          int(u.get("output_tokens") or 0),
        "cache_read_tokens":      int(u.get("cache_read_input_tokens") or 0),
        "cache_create_5m_tokens": int(cc.get("ephemeral_5m_input_tokens") or 0),
        "cache_create_1h_tokens": int(cc.get("ephemeral_1h_input_tokens") or 0),
    }


def _prompt_text(rec: dict) -> Tuple[Optional[str], Optional[int]]:
    if rec.get("type") != "user":
        return None, None
    content = (rec.get("message") or {}).get("content")
    if isinstance(content, str):
        return content, len(content)
    if isinstance(content, list):
        parts = [b.get("text", "") for b in content if isinstance(b, dict) and b.get("type") == "text"]
        text = "".join(parts) if parts else None
        return text, (len(text) if text else None)
    return None, None


def _target(name: str, inp: dict) -> Optional[str]:
    field = _TARGET_FIELDS.get(name)
    if field and isinstance(inp, dict):
        v = inp.get(field)
        if isinstance(v, str):
            return v[:500]
    return None


def _extract_tools(rec: dict) -> List[dict]:
    out = []
    content = (rec.get("message") or {}).get("content")
    if not isinstance(content, list):
        return out
    for block in content:
        if not isinstance(block, dict) or block.get("type") != "tool_use":
            continue
        name = block.get("name") or "unknown"
        target = _target(name, block.get("input") or {})
        out.append({
            "tool_name":     name,
            "target":        target,
            "result_tokens": None,
            "is_error":      0,
            "timestamp":     rec.get("timestamp"),
        })
    return out


def _extract_results(rec: dict) -> List[dict]:
    out = []
    content = (rec.get("message") or {}).get("content")
    if not isinstance(content, list):
        return out
    for block in content:
        if not isinstance(block, dict) or block.get("type") != "tool_result":
            continue
        body = block.get("content")
        if isinstance(body, str):
            chars = len(body)
        elif isinstance(body, list):
            chars = sum(len(p.get("text", "")) for p in body if isinstance(p, dict))
        else:
            chars = 0
        out.append({
            "tool_name":     "_tool_result",
            "target":        block.get("tool_use_id"),
            "result_tokens": chars // 4,
            "is_error":      1 if block.get("is_error") else 0,
            "timestamp":     rec.get("timestamp"),
        })
    return out


def parse_record(rec: dict, project_slug: str, source: str = "claude") -> Tuple[dict, List[dict]]:
    """Return (message_row, [tool_call_rows])."""
    msg_obj = rec.get("message") or {}
    text, chars = _prompt_text(rec)
    msg = {
        "uuid":         rec.get("uuid"),
        "parent_uuid":  rec.get("parentUuid"),
        "session_id":   rec.get("sessionId"),
        "project_slug": project_slug,
        "cwd":          rec.get("cwd"),
        "git_branch":   rec.get("gitBranch"),
        "cc_version":   rec.get("version"),
        "entrypoint":   rec.get("entrypoint"),
        "type":         rec.get("type"),
        "is_sidechain": 1 if rec.get("isSidechain") else 0,
        "agent_id":     rec.get("agentId"),
        "timestamp":    rec.get("timestamp"),
        "model":        msg_obj.get("model"),
        "stop_reason":  msg_obj.get("stop_reason"),
        "prompt_id":    rec.get("promptId"),
        "message_id":   msg_obj.get("id"),
        "prompt_text":  text,
        "prompt_chars": chars,
        "tool_calls_json": None,
        "source":       source,
        **_usage(rec),
    }
    tools = _extract_tools(rec)
    tools.extend(_extract_results(rec))
    if tools:
        msg["tool_calls_json"] = json.dumps(
            [{"name": t["tool_name"], "target": t["target"]} for t in tools if t["tool_name"] != "_tool_result"]
        )
    for t in tools:
        t["message_uuid"] = msg["uuid"]
        t["session_id"]   = msg["session_id"]
        t["project_slug"] = project_slug
        t["source"]       = source
    return msg, tools


def _project_slug(file_path: Path, projects_root: Path) -> str:
    rel = file_path.relative_to(projects_root)
    return rel.parts[0]


def _evict_prior_snapshots(conn, session_id: str, message_id: str, keep_uuid: str) -> None:
    """Remove older streaming snapshots for the same (session_id, message_id).

    Claude Code writes 2–3 JSONL lines per assistant response (partial → final)
    with identical message.id but distinct top-level uuids. Only the final
    tally matches billing, so earlier snapshots must be replaced, not summed.
    """
    old = [r[0] for r in conn.execute(
        "SELECT uuid FROM messages WHERE session_id=? AND message_id=? AND uuid!=?",
        (session_id, message_id, keep_uuid),
    )]
    if not old:
        return
    placeholders = ",".join("?" * len(old))
    conn.execute(f"DELETE FROM tool_calls WHERE message_uuid IN ({placeholders})", old)
    conn.execute(f"DELETE FROM messages WHERE uuid IN ({placeholders})", old)


def scan_file(path: Path, project_slug: str, conn, start_byte: int = 0, source: str = "claude") -> dict:
    """Ingest new lines from a JSONL file starting at ``start_byte``.

    Returns message/tool counts plus ``end_offset`` — the byte offset just
    past the last fully-parsed line. Callers persist ``end_offset`` as the
    file's high-water mark so a line partially flushed at EOF gets re-read
    once it completes.
    """
    msgs = tools = 0
    end_offset = start_byte
    with open(path, "rb") as fb:
        if start_byte:
            fb.seek(start_byte)
        while True:
            raw = fb.readline()
            if not raw:
                break  # EOF
            if not raw.endswith(b"\n"):
                # Partial line — Claude Code is mid-flush. Leave the
                # high-water mark behind the line start so we re-read it
                # once the write completes.
                break
            line_end = fb.tell()
            try:
                line = raw.decode("utf-8", errors="replace").strip()
            except Exception:
                end_offset = line_end
                continue
            if not line:
                end_offset = line_end
                continue
            try:
                rec = json.loads(line)
            except json.JSONDecodeError:
                end_offset = line_end
                continue
            if not isinstance(rec, dict) or "uuid" not in rec or "type" not in rec:
                end_offset = line_end
                continue
            msg, tlist = parse_record(rec, project_slug, source)
            if not msg["session_id"] or not msg["timestamp"]:
                end_offset = line_end
                continue
            if msg["message_id"]:
                _evict_prior_snapshots(conn, msg["session_id"], msg["message_id"], msg["uuid"])
            conn.execute(INSERT_MSG, msg)
            # tool_calls has no natural unique key; clear any prior rows for
            # this uuid so full rescans stay idempotent instead of
            # duplicating rows.
            conn.execute("DELETE FROM tool_calls WHERE message_uuid=?", (msg["uuid"],))
            for t in tlist:
                conn.execute(INSERT_TOOL, t)
                tools += 1
            msgs += 1
            end_offset = line_end
    return {"messages": msgs, "tools": tools, "end_offset": end_offset}


# ---------------------------------------------------------------------------
# Other agents. Every parser turns its native log into the same `messages` /
# `tool_calls` rows, tagged with `source`, so the dashboard can show Claude
# Code, Codex and anything else in one place.
# ---------------------------------------------------------------------------

def _iter_lines(path: Path, start_byte: int):
    """Yield (line_start, line_end, record|None) for each complete line.

    Same high-water-mark contract as scan_file: a partial trailing line is
    left unread so it is re-read once the writer finishes it.
    """
    with open(path, "rb") as fb:
        if start_byte:
            fb.seek(start_byte)
        while True:
            line_start = fb.tell()
            raw = fb.readline()
            if not raw or not raw.endswith(b"\n"):
                return
            try:
                rec = json.loads(raw.decode("utf-8", errors="replace"))
            except json.JSONDecodeError:
                rec = None
            yield line_start, fb.tell(), (rec if isinstance(rec, dict) else None)


def _blank_msg(source: str, **kw) -> dict:
    row = {
        "uuid": None, "parent_uuid": None, "session_id": None, "project_slug": "",
        "cwd": None, "git_branch": None, "cc_version": None, "entrypoint": None,
        "type": "assistant", "is_sidechain": 0, "agent_id": None, "timestamp": None,
        "model": None, "stop_reason": None, "prompt_id": None, "message_id": None,
        "input_tokens": 0, "output_tokens": 0, "cache_read_tokens": 0,
        "cache_create_5m_tokens": 0, "cache_create_1h_tokens": 0,
        "prompt_text": None, "prompt_chars": None, "tool_calls_json": None,
        "source": source,
    }
    row.update(kw)
    return row


_UUID_TAIL = re.compile(r"([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$")


def _codex_target(args_json: str) -> Optional[str]:
    try:
        a = json.loads(args_json)
    except (TypeError, ValueError):
        return None
    if isinstance(a, dict):
        for k in ("cmd", "command", "path", "file_path", "pattern", "query", "url", "task_name"):
            v = a.get(k)
            if isinstance(v, list):
                v = " ".join(str(x) for x in v)
            if isinstance(v, str):
                return v[:500]
    return None


_JS_TOOL = re.compile(r"tools\.([A-Za-z0-9_]+)\(")
_JS_CMD = re.compile(r'"cmd"\s*:\s*"((?:[^"\\]|\\.)*)"')
_PATCH_FILE = re.compile(r"\*\*\* (?:Update|Add|Delete) File: ([^\n\\\"]+)")
_SKILL_MD = re.compile(r"skills/([^/\s\"']+)/SKILL\.md")
_EXIT_CODE = re.compile(r"(?:Process exited with code|exit code|exited with code)[: ]+(-?\d+)", re.I)

# Codex tool -> the Claude Code name for the same job, so Tools / Profile / Tips
# compare agents instead of treating every Codex tool as foreign. Anything not
# listed (incl. mcp__*) keeps its native name.
CODEX_TOOL_CLASS = {
    "exec_command": "Bash", "write_stdin": "Bash", "shell": "Bash", "local_shell": "Bash",
    "apply_patch": "Edit", "view_image": "Read",
    "web__run": "WebSearch", "web_search": "WebSearch",
    "spawn_agent": "Task",
}


def _unjs(s: str) -> str:
    """Undo JS/JSON string escapes in a captured literal (best effort)."""
    try:
        return json.loads(f'"{s}"')
    except ValueError:
        return s


def _codex_calls(payload: dict) -> List[Tuple[str, Optional[str]]]:
    """(tool_class, target) per underlying tool call in one Codex response item.

    Codex's `exec` custom tool is a JS snippet that calls tools.exec_command /
    tools.apply_patch / ... one or more times; the real tool names live in the
    source. Plain function_calls carry JSON arguments.
    """
    name, pt = payload.get("name") or "unknown", payload.get("type")
    out: List[Tuple[str, Optional[str]]] = []
    if pt == "function_call":
        out.append((CODEX_TOOL_CLASS.get(name, name), _codex_target(payload.get("arguments"))))
    else:
        src = payload.get("input") or ""
        cmds = [_unjs(c) for c in _JS_CMD.findall(src)]
        names = _JS_TOOL.findall(src)
        for n in names:
            if n == "apply_patch":
                files = _PATCH_FILE.findall(src) or [None]
                out.extend(("Edit", f) for f in files)
            elif n == "exec_command":
                out.append(("Bash", (cmds.pop(0) if cmds else None)))
            else:
                out.append((CODEX_TOOL_CLASS.get(n, n), None))
        if not names:
            out.append((CODEX_TOOL_CLASS.get(name, name), None))
    # Codex has no Skill tool; a skill is "used" when its SKILL.md is read.
    for cls, tgt in list(out):
        if cls == "Bash" and tgt:
            for sk in _SKILL_MD.findall(tgt):
                out.append(("Skill", sk))
    return [(c, (t[:500] if isinstance(t, str) else t)) for c, t in out]


def _codex_output(payload: dict) -> Tuple[int, int]:
    """(approx_tokens, is_error) for a function/custom tool output item."""
    body = payload.get("output")
    if isinstance(body, list):
        body = "".join(b.get("text", "") for b in body if isinstance(b, dict))
    body = body if isinstance(body, str) else json.dumps(body or "")
    code = _EXIT_CODE.search(body)
    return len(body) // 4, int(bool(code and int(code.group(1)) != 0))


def scan_codex_file(path: Path, root: Path, conn, start_byte: int = 0,
                    state: Optional[dict] = None, source: str = "codex") -> dict:
    """Ingest an OpenAI Codex CLI/desktop rollout (~/.codex/sessions/**/rollout-*.jsonl).

    One assistant row per API call, from `token_count` events. Codex repeats
    the same event (identical cumulative total) when nothing new was billed,
    so only events whose cumulative total moved are counted; verified: the
    deduped per-call sum equals the final cumulative total exactly.
    Codex's `input_tokens` already includes cached tokens, so cached tokens
    are split out to cache_read and removed from input to avoid double-billing.
    """
    st = dict(state or {})
    m = _UUID_TAIL.search(path.name)
    # tid (from the filename, unique per file) keys rows. sid groups a session:
    # subagents share their parent's, as Claude sidechains do. Only the FIRST
    # session_meta describes this file; forks embed further metas for inherited
    # history, and trusting those collided keys across files.
    tid = m.group(1) if m else path.stem
    sid = st.get("sid") or tid
    msgs = tools = 0
    end = start_byte
    pending: List[dict] = []  # tool calls since the last billed API call
    for line_start, line_end, rec in _iter_lines(path, start_byte):
        end = line_end
        if rec is None:
            continue
        t, p, ts = rec.get("type"), rec.get("payload") or {}, rec.get("timestamp")
        ordinal = rec.get("ordinal")
        key = f"{source}:{tid}:" + (str(ordinal) if ordinal is not None else f"b{line_start}")
        slug = lambda: _encode_slug(st["cwd"]) if st.get("cwd") else f"{source}-unknown"
        if t == "session_meta" and not st.get("meta_seen"):
            sid = p.get("session_id") or p.get("id") or tid
            st.update(meta_seen=True, sid=sid, cwd=p.get("cwd") or st.get("cwd"),
                      version=p.get("cli_version"), entrypoint=p.get("originator"),
                      sidechain=int(p.get("thread_source") == "subagent"),
                      agent=p.get("agent_nickname"))
        elif t == "turn_context":
            st["model"] = p.get("model") or st.get("model")
            st["cwd"] = p.get("cwd") or st.get("cwd")
        elif t == "response_item" and ts:
            pt = p.get("type")
            if pt == "message" and p.get("role") == "user":
                text = "".join(b.get("text", "") for b in (p.get("content") or [])
                               if isinstance(b, dict) and b.get("type") == "input_text")
                # Codex injects context (AGENTS.md, environment) as user messages.
                if text and not text.lstrip().startswith(("<", "# AGENTS.md")):
                    conn.execute(INSERT_MSG, _blank_msg(
                        source, uuid=key, session_id=sid, project_slug=slug(), cwd=st.get("cwd"),
                        cc_version=st.get("version"), entrypoint=st.get("entrypoint"), type="user",
                        is_sidechain=st.get("sidechain", 0), agent_id=st.get("agent"), timestamp=ts, prompt_text=text, prompt_chars=len(text)))
                    st["last_user"] = key
                    msgs += 1
            elif pt in ("function_call", "custom_tool_call"):
                for cls, tgt in _codex_calls(p):
                    conn.execute(INSERT_TOOL, {
                        "message_uuid": key, "session_id": sid, "project_slug": slug(),
                        "tool_name": cls, "target": tgt, "result_tokens": None,
                        "is_error": 0, "timestamp": ts, "source": source})
                    pending.append({"name": cls, "target": tgt})
                    tools += 1
            elif pt in ("function_call_output", "custom_tool_call_output"):
                toks, err = _codex_output(p)
                conn.execute(INSERT_TOOL, {
                    "message_uuid": key, "session_id": sid, "project_slug": slug(),
                    "tool_name": "_tool_result", "target": p.get("call_id"),
                    "result_tokens": toks, "is_error": err, "timestamp": ts, "source": source})
                tools += 1
        elif t == "event_msg" and ts and p.get("type") == "token_count":
            info = p.get("info") or {}
            total, last = info.get("total_token_usage"), info.get("last_token_usage") or {}
            if not total or total == st.get("last_total"):
                continue
            st["last_total"] = total
            cached = int(last.get("cached_input_tokens") or 0)
            conn.execute(INSERT_MSG, _blank_msg(
                source, uuid=key, parent_uuid=st.get("last_user"), session_id=sid,
                project_slug=slug(), cwd=st.get("cwd"), cc_version=st.get("version"),
                entrypoint=st.get("entrypoint"), is_sidechain=st.get("sidechain", 0),
                agent_id=st.get("agent"), timestamp=ts, model=st.get("model"),
                input_tokens=max(int(last.get("input_tokens") or 0) - cached, 0),
                output_tokens=int(last.get("output_tokens") or 0),
                cache_read_tokens=cached,
                tool_calls_json=json.dumps(pending) if pending else None))
            pending = []
            msgs += 1
    st["sid"] = sid
    return {"messages": msgs, "tools": tools, "end_offset": end, "state": st}


def scan_generic_file(path: Path, root: Path, conn, start_byte: int = 0,
                      state: Optional[dict] = None, source: str = "generic") -> dict:
    """Ingest a normalised JSONL any agent can emit, one object per API call:

        {"session_id", "timestamp", "model", "input_tokens", "output_tokens",
         "cache_read_tokens"?, "cwd"?, "project"?, "prompt"?, "tools"?: ["name", ...]}

    `input_tokens` here means uncached input. Required: session_id, timestamp.
    """
    msgs = tools = 0
    end = start_byte
    tag = hashlib.sha1(str(path).encode()).hexdigest()[:8]
    for line_start, line_end, rec in _iter_lines(path, start_byte):
        end = line_end
        if not rec or not rec.get("session_id") or not rec.get("timestamp"):
            continue
        sid, ts = str(rec["session_id"]), rec["timestamp"]
        cwd = rec.get("cwd")
        slug = rec.get("project") or (_encode_slug(cwd) if cwd else f"{source}-unknown")
        key = f"{source}:{tag}:{line_start}"
        parent = None
        if rec.get("prompt"):
            parent = key + ":u"
            conn.execute(INSERT_MSG, _blank_msg(
                source, uuid=parent, session_id=sid, project_slug=slug, cwd=cwd, type="user",
                timestamp=ts, prompt_text=str(rec["prompt"]), prompt_chars=len(str(rec["prompt"]))))
            msgs += 1
        names = [str(n) for n in (rec.get("tools") or [])]
        conn.execute(INSERT_MSG, _blank_msg(
            source, uuid=key, parent_uuid=parent, session_id=sid, project_slug=slug, cwd=cwd,
            timestamp=ts, model=rec.get("model"),
            input_tokens=int(rec.get("input_tokens") or 0),
            output_tokens=int(rec.get("output_tokens") or 0),
            cache_read_tokens=int(rec.get("cache_read_tokens") or 0),
            tool_calls_json=json.dumps([{"name": n, "target": None} for n in names]) if names else None))
        msgs += 1
        for n in names:
            conn.execute(INSERT_TOOL, {
                "message_uuid": key, "session_id": sid, "project_slug": slug, "tool_name": n,
                "target": None, "result_tokens": None, "is_error": 0, "timestamp": ts, "source": source})
            tools += 1
    return {"messages": msgs, "tools": tools, "end_offset": end, "state": state or {}}


def _scan_claude_file(path, root, conn, start_byte=0, state=None, source="claude"):
    sub = scan_file(path, _project_slug(path, root), conn, start_byte=start_byte, source=source)
    sub["state"] = state or {}
    return sub


# ---------------------------------------------------------------------------
# Hermes (SQLite, not JSONL): ~/.hermes/state.db plus one state.db per profile.
# ---------------------------------------------------------------------------

HERMES_TOOL_CLASS = {
    "terminal": "Bash", "read_file": "Read", "write_file": "Write", "patch": "Edit",
    "search_files": "Grep", "skill_view": "Skill", "delegate_task": "Task",
    "web_search": "WebSearch", "web_extract": "WebFetch",
}
_HERMES_TARGET_KEYS = ("command", "path", "file_path", "pattern", "name", "query", "url", "goal", "task")


def _iso(ts: float) -> str:
    return datetime.fromtimestamp(ts, timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def _hermes_dbs(root: Path) -> List[Tuple[str, Path]]:
    """[(label, db_path)]: the main DB is 'main', profile DBs are named by profile."""
    if root.is_file():
        return [("main", root)]
    out = [("main", root / "state.db")] if (root / "state.db").is_file() else []
    out += [(d.name, d / "state.db") for d in sorted((root / "profiles").glob("*")) if (d / "state.db").is_file()]
    return out


def _hermes_tool_calls(raw: Optional[str]) -> List[Tuple[str, Optional[str]]]:
    try:
        calls = json.loads(raw) if raw else []
    except ValueError:
        return []
    out = []
    for c in calls if isinstance(calls, list) else []:
        fn = (c or {}).get("function") or {}
        name = fn.get("name") or "unknown"
        target = None
        try:
            args = json.loads(fn.get("arguments") or "{}")
        except ValueError:
            args = {}
        if isinstance(args, dict):
            for k in _HERMES_TARGET_KEYS:
                if isinstance(args.get(k), str):
                    target = args[k][:500]
                    break
        out.append((HERMES_TOOL_CLASS.get(name, name), target))
    return out


def _hermes_result(content: Optional[str]) -> Tuple[int, int]:
    body = content or ""
    err = 0
    try:
        j = json.loads(body)
        if isinstance(j, dict):
            err = int(j.get("status") == "error" or bool(j.get("error")) or j.get("exit_code") not in (None, 0))
    except ValueError:
        pass
    return len(body) // 4, err


def _ingest_hermes_session(conn, label, ses, msgs, alias, name) -> Tuple[int, int]:
    """Write one session. Hermes keeps only per-session token totals (no per-call
    usage), so the totals are split evenly across the session's assistant turns:
    session sums are exact, per-turn and per-day figures are an approximation."""
    sid = f"{name}:{label}:{ses['id']}"
    cwd = ses["cwd"]
    slug = _encode_slug(cwd) if cwd else f"{name}-unknown"
    model = alias.get(ses["model"], ses["model"])
    common = dict(session_id=ses["id"], project_slug=slug, cwd=cwd, git_branch=ses["git_branch"],
                  entrypoint=ses["source"], agent_id=None if label == "main" else label)
    asst = [m for m in msgs if m["role"] == "assistant"]
    totals = {"input_tokens": ses["input_tokens"] or 0, "output_tokens": ses["output_tokens"] or 0,
              "cache_read_tokens": ses["cache_read_tokens"] or 0,
              "cache_create_5m_tokens": ses["cache_write_tokens"] or 0}
    n = max(len(asst), 1) if any(totals.values()) else len(asst)
    nmsg = ntool = 0
    last_user, seen_asst = None, 0
    ordered = list(msgs)
    if not asst and any(totals.values()):  # tokens but no recorded assistant message
        ordered.append({"id": 0, "role": "assistant", "content": None, "tool_calls": None,
                        "tool_name": None, "tool_call_id": None, "timestamp": ses["started_at"]})
    for m in ordered:
        key, ts = f"{sid}:{m['id']}", _iso(m["timestamp"])
        if m["role"] == "user" and isinstance(m["content"], str) and m["content"].strip():
            conn.execute(INSERT_MSG, _blank_msg(name, uuid=key, timestamp=ts, type="user", **{
                **common, "prompt_text": m["content"], "prompt_chars": len(m["content"])}))
            last_user, nmsg = key, nmsg + 1
        elif m["role"] == "assistant":
            seen_asst += 1
            part = {k: v // n + (v % n if seen_asst == n else 0) for k, v in totals.items()}
            calls = _hermes_tool_calls(m["tool_calls"])
            conn.execute(INSERT_MSG, _blank_msg(name, uuid=key, parent_uuid=last_user, timestamp=ts, model=model,
                         tool_calls_json=json.dumps([{"name": c, "target": t} for c, t in calls]) if calls else None,
                         **{**common, **part}))
            nmsg += 1
            for cls, tgt in calls:
                conn.execute(INSERT_TOOL, {"message_uuid": key, "session_id": ses["id"], "project_slug": slug,
                             "tool_name": cls, "target": tgt, "result_tokens": None, "is_error": 0,
                             "timestamp": ts, "source": name})
                ntool += 1
        elif m["role"] == "tool":
            toks, err = _hermes_result(m["content"])
            conn.execute(INSERT_TOOL, {"message_uuid": key, "session_id": ses["id"], "project_slug": slug,
                         "tool_name": "_tool_result", "target": m["tool_call_id"], "result_tokens": toks,
                         "is_error": err, "timestamp": ts, "source": name})
            ntool += 1
    return nmsg, ntool


def scan_hermes(name: str, root: Union[str, Path], db_path: Union[str, Path], options: Optional[dict] = None) -> dict:
    """Ingest Hermes sessions. A session is re-ingested whenever its totals or
    message count change (its old rows are replaced), so live sessions stay current."""
    totals = {"messages": 0, "tools": 0, "files": 0}
    alias = dict((options or {}).get("model_alias") or {})
    alias_sig = json.dumps(alias, sort_keys=True)
    with connect(db_path) as conn:
        for label, dbp in _hermes_dbs(Path(root).expanduser()):
            try:
                ro = sqlite3.connect(f"file:{dbp}?mode=ro", uri=True, timeout=5)
                ro.row_factory = sqlite3.Row
                sessions = ro.execute("SELECT * FROM sessions").fetchall()
            except sqlite3.Error:
                continue  # locked or mid-migration: pick it up next scan
            row = conn.execute("SELECT state FROM files WHERE path=?", (str(dbp),)).fetchone()
            prev = json.loads(row["state"]) if row and row["state"] else {}
            sigs = prev.get("sigs", {}) if prev.get("alias") == alias_sig else {}
            new_sigs = {}
            for ses in sessions:
                sig = [ses["message_count"], ses["input_tokens"], ses["output_tokens"],
                       ses["cache_read_tokens"], ses["ended_at"], ses["api_call_count"]]
                new_sigs[ses["id"]] = sig
                if sigs.get(ses["id"]) == sig:
                    continue
                prefix = f"{name}:{label}:{ses['id']}:"
                conn.execute("DELETE FROM tool_calls WHERE message_uuid LIKE ? ESCAPE '\\'", (prefix.replace("_", "\\_") + "%",))
                conn.execute("DELETE FROM messages WHERE uuid LIKE ? ESCAPE '\\'", (prefix.replace("_", "\\_") + "%",))
                msgs = ro.execute("SELECT id, role, content, tool_calls, tool_name, tool_call_id, timestamp "
                                  "FROM messages WHERE session_id=? ORDER BY timestamp, id", (ses["id"],)).fetchall()
                m, t = _ingest_hermes_session(conn, label, ses, [dict(x) for x in msgs], alias, name)
                totals["messages"] += m
                totals["tools"] += t
            ro.close()
            conn.execute("INSERT OR REPLACE INTO files (path, mtime, bytes_read, scanned_at, state) VALUES (?,?,?,?,?)",
                         (str(dbp), dbp.stat().st_mtime, 0, time.time(), json.dumps({"sigs": new_sigs, "alias": alias_sig})))
            conn.commit()
            totals["files"] += 1
    return totals


# kind -> (parser, glob). New agents are one entry here plus a parser above.
KINDS = {
    "hermes":  (None,               None),   # SQLite; handled by scan_hermes
    "claude":  (_scan_claude_file,  "*.jsonl"),
    "codex":   (scan_codex_file,    "*.jsonl"),
    "generic": (scan_generic_file,  "*.jsonl"),
}


class Progress:
    """Live counters for the first-run setup screen (/api/scan-status)."""

    def __init__(self):
        self._lock = threading.Lock()
        self.running = False
        self.files_total = self.files_done = self.messages = self.tools = 0
        self.current = ""
        self.started_ms = self.finished_ms = 0
        self.error = ""

    def begin(self):
        with self._lock:
            self.running = True
            self.files_total = self.files_done = self.messages = self.tools = 0
            self.current, self.error = "", ""
            self.started_ms, self.finished_ms = int(time.time() * 1000), 0

    def end(self, error: str = ""):
        with self._lock:
            self.running = False
            self.error = error
            self.finished_ms = int(time.time() * 1000)

    def add_total(self, n: int):
        with self._lock:
            self.files_total += n

    def tick(self, current: str = "", messages: int = 0, tools: int = 0):
        with self._lock:
            self.files_done += 1
            self.messages += messages
            self.tools += tools
            if current:
                self.current = current

    def snapshot(self) -> dict:
        with self._lock:
            return {"running": self.running, "files_total": self.files_total, "files_done": self.files_done,
                    "messages": self.messages, "tools": self.tools, "current": self.current,
                    "started_ms": self.started_ms, "finished_ms": self.finished_ms, "error": self.error}


def scan_source(name: str, kind: str, root: Union[str, Path], db_path: Union[str, Path],
                options: Optional[dict] = None, progress: Optional[Progress] = None) -> dict:
    totals = {"messages": 0, "tools": 0, "files": 0}
    if kind not in KINDS:
        raise ValueError(f"unknown source kind {kind!r}; expected one of {sorted(KINDS)}")
    if kind == "hermes":
        if progress:
            progress.add_total(1)
        t = scan_hermes(name, root, db_path, options)
        if progress:
            progress.tick(str(root), t.get("messages", 0), t.get("tools", 0))
        return t
    parser, _ = KINDS[kind]
    root = Path(root).expanduser()
    if root.is_file():
        files, base = [root], root.parent
    elif root.is_dir():
        files, base = root.rglob("*.jsonl"), root
    else:
        return totals
    if progress:
        files = list(files)
        progress.add_total(len(files))
    with connect(db_path) as conn:
        for p in files:
            try:
                stat = p.stat()
            except OSError:
                if progress:
                    progress.tick()
                continue
            row = conn.execute(
                "SELECT mtime, bytes_read, state FROM files WHERE path=?", (str(p),)
            ).fetchone()
            offset, state = 0, None
            if row and row["mtime"] == stat.st_mtime and row["bytes_read"] == stat.st_size:
                if progress:
                    progress.tick()  # unchanged files still count toward files_done
                continue
            if row and stat.st_size > row["bytes_read"]:
                offset = row["bytes_read"]
                state = json.loads(row["state"]) if row["state"] else None
            sub = parser(p, base, conn, offset, state, name)
            # Persist the byte offset of the last fully-parsed line (not
            # st_size) so a partial line mid-flush is retried on the next
            # scan instead of being skipped over.
            conn.execute(
                "INSERT OR REPLACE INTO files (path, mtime, bytes_read, scanned_at, state) VALUES (?, ?, ?, ?, ?)",
                (str(p), stat.st_mtime, sub["end_offset"], time.time(), json.dumps(sub["state"])),
            )
            conn.commit()
            totals["messages"] += sub["messages"]
            totals["tools"]    += sub["tools"]
            totals["files"]    += 1
            if progress:
                progress.tick(p.name, sub["messages"], sub["tools"])
    return totals


def scan_dir(projects_root: Union[str, Path], db_path: Union[str, Path]) -> dict:
    """Scan Claude Code transcripts only (kept for callers that predate multi-agent)."""
    return scan_source("claude", "claude", projects_root, db_path)


def scan_sources(sources, db_path: Union[str, Path], progress: Optional[Progress] = None) -> dict:
    """Scan every configured source; sources = [{"name","kind","path"}, ...]."""
    totals = {"messages": 0, "tools": 0, "files": 0}
    if progress:
        progress.begin()
    err = ""
    try:
        for s in sources:
            n = scan_source(s["name"], s["kind"], s["path"], db_path, s.get("options"), progress)
            for k in totals:
                totals[k] += n[k]
    except Exception as e:
        err = str(e)
        raise
    finally:
        if progress:
            progress.end(err)
    return totals
