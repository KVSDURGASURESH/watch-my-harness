import json
import os
import tempfile
import unittest
from pathlib import Path

from token_dashboard.db import init_db, connect, overview_totals, model_breakdown, source_breakdown, tool_token_breakdown
from token_dashboard.pricing import cost_for
from token_dashboard.scanner import scan_source, scan_sources
from token_dashboard.sources import parse_source_arg, resolve_sources

SID = "01a03549-440a-7b53-b769-16c8a6a19ba0"


def _tc(ordinal, ts, total, last):
    return {"timestamp": ts, "ordinal": ordinal, "type": "event_msg",
            "payload": {"type": "token_count", "info": {"total_token_usage": total, "last_token_usage": last}}}


def _usage(i, c, o):
    return {"input_tokens": i, "cached_input_tokens": c, "output_tokens": o, "total_tokens": i + o}


def codex_lines():
    return [
        {"timestamp": "2026-08-24T19:39:49Z", "ordinal": 0, "type": "session_meta",
         "payload": {"id": SID, "cwd": "/Users/x/proj", "cli_version": "0.1", "originator": "codex_exec"}},
        {"timestamp": "2026-08-24T19:39:50Z", "ordinal": 1, "type": "turn_context",
         "payload": {"model": "gpt-test", "cwd": "/Users/x/proj"}},
        {"timestamp": "2026-08-24T19:39:50Z", "ordinal": 2, "type": "response_item",
         "payload": {"type": "message", "role": "user",
                     "content": [{"type": "input_text", "text": "<environment_context>noise</environment_context>"}]}},
        {"timestamp": "2026-08-24T19:39:51Z", "ordinal": 3, "type": "response_item",
         "payload": {"type": "message", "role": "user",
                     "content": [{"type": "input_text", "text": "fix the bug"}]}},
        {"timestamp": "2026-08-24T19:39:52Z", "ordinal": 4, "type": "response_item",
         "payload": {"type": "function_call", "name": "shell", "arguments": json.dumps({"cmd": "ls -la"})}},
        _tc(5, "2026-08-24T19:39:53Z", _usage(1000, 400, 50), _usage(1000, 400, 50)),
        _tc(6, "2026-08-24T19:39:54Z", _usage(1000, 400, 50), _usage(1000, 400, 50)),  # repeat: must not double count
        _tc(7, "2026-08-24T19:39:55Z", _usage(1700, 600, 80), _usage(700, 200, 30)),
    ]


def write_jsonl(path, rows):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps(r) + "\n" for r in rows), encoding="utf-8")


class CodexScanTests(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.db = str(self.tmp / "t.db")
        init_db(self.db)
        self.root = self.tmp / "codex"
        self.f = self.root / "2026" / "08" / "24" / f"rollout-2026-08-24T12-39-49-{SID}.jsonl"
        write_jsonl(self.f, codex_lines())

    def test_tokens_dedupe_and_cache_split(self):
        scan_source("codex", "codex", self.root, self.db)
        t = overview_totals(self.db, source="codex")
        # final cumulative: input 1700 incl. 600 cached, output 80
        self.assertEqual(t["input_tokens"], 1700 - 600)
        self.assertEqual(t["cache_read_tokens"], 600)
        self.assertEqual(t["output_tokens"], 80)
        self.assertEqual(t["sessions"], 1)
        self.assertEqual(t["turns"], 1)  # injected <environment_context> is not a prompt

    def test_model_project_and_tools(self):
        scan_source("codex", "codex", self.root, self.db)
        self.assertEqual(model_breakdown(self.db, source="codex")[0]["model"], "gpt-test")
        with connect(self.db) as c:
            slug = c.execute("SELECT DISTINCT project_slug FROM messages").fetchone()[0]
            self.assertEqual(slug, "-Users-x-proj")  # same encoding Claude Code uses
        tools = tool_token_breakdown(self.db, source="codex")
        self.assertEqual([(r["tool_name"], r["calls"]) for r in tools], [("Bash", 1)])  # Codex shell is normalised to Claude's Bash name

    def test_incremental_resume_keeps_model_and_dedupe_state(self):
        scan_source("codex", "codex", self.root, self.db)
        with open(self.f, "a") as fh:
            fh.write(json.dumps(_tc(8, "2026-08-24T19:40:00Z", _usage(1700, 600, 80), _usage(700, 200, 30))) + "\n")  # repeat of last total
            fh.write(json.dumps(_tc(9, "2026-08-24T19:40:01Z", _usage(2000, 700, 100), _usage(300, 100, 20))) + "\n")
        os.utime(self.f, (self.f.stat().st_atime, self.f.stat().st_mtime + 5))
        n = scan_source("codex", "codex", self.root, self.db)
        self.assertEqual(n["messages"], 1)  # only the new, non-repeat event
        rows = model_breakdown(self.db, source="codex")
        self.assertEqual(rows[0]["model"], "gpt-test")  # model survived the resume via files.state
        self.assertEqual(rows[0]["output_tokens"], 100)

    def test_rescan_is_idempotent(self):
        scan_source("codex", "codex", self.root, self.db)
        with connect(self.db) as c:
            c.execute("DELETE FROM files")
            c.commit()
        scan_source("codex", "codex", self.root, self.db)
        self.assertEqual(overview_totals(self.db, source="codex")["output_tokens"], 80)

    def test_unpriced_model_has_no_cost_not_zero(self):
        scan_source("codex", "codex", self.root, self.db)
        m = model_breakdown(self.db, source="codex")[0]
        pricing = {"models": {}, "tier_fallback": {}}
        self.assertIsNone(cost_for(m["model"], m, pricing)["usd"])
        pricing["models"]["gpt-test"] = {"input": 1.0, "output": 2.0}  # cache rates optional
        self.assertIsNotNone(cost_for(m["model"], m, pricing)["usd"])


class CodexToolTests(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.db = str(self.tmp / "t.db")
        init_db(self.db)
        js = ('const r = await tools.exec_command({"cmd":"sed -n 1,9p /h/skills/brainstorming/SKILL.md","workdir":"/a"});'
              ' text(await tools.apply_patch("*** Begin Patch\\n*** Update File: /a/b.py\\n@@\\n+x\\n"));')
        rows = codex_lines()[:4] + [
            {"timestamp": "2026-08-24T19:39:52Z", "ordinal": 4, "type": "response_item",
             "payload": {"type": "custom_tool_call", "name": "exec", "call_id": "c1", "input": js}},
            {"timestamp": "2026-08-24T19:39:52Z", "ordinal": 5, "type": "response_item",
             "payload": {"type": "custom_tool_call_output", "call_id": "c1",
                         "output": [{"type": "input_text", "text": "x" * 400 + "\nProcess exited with code 2"}]}},
            _tc(6, "2026-08-24T19:39:53Z", _usage(1000, 400, 50), _usage(1000, 400, 50))]
        self.root = self.tmp / "codex"
        write_jsonl(self.root / f"rollout-{SID}.jsonl", rows)
        scan_source("codex", "codex", self.root, self.db)

    def test_inner_tools_normalised_to_claude_names(self):
        with connect(self.db) as c:
            got = sorted((r["tool_name"], r["target"]) for r in c.execute(
                "SELECT tool_name, target FROM tool_calls WHERE tool_name != '_tool_result'"))
        self.assertEqual(got, [("Bash", "sed -n 1,9p /h/skills/brainstorming/SKILL.md"),
                               ("Edit", "/a/b.py"), ("Skill", "brainstorming")])

    def test_result_rows_carry_size_and_exit_status(self):
        with connect(self.db) as c:
            r = c.execute("SELECT result_tokens, is_error, target FROM tool_calls WHERE tool_name='_tool_result'").fetchone()
        self.assertEqual((r["is_error"], r["target"]), (1, "c1"))
        self.assertGreater(r["result_tokens"], 90)

    def test_assistant_row_lists_its_tools(self):
        with connect(self.db) as c:
            tj = c.execute("SELECT tool_calls_json FROM messages WHERE type='assistant'").fetchone()[0]
        self.assertEqual({t["name"] for t in json.loads(tj)}, {"Bash", "Edit", "Skill"})

    def test_profile_and_tips_accept_source(self):
        from token_dashboard.profile import build_profile
        from token_dashboard.tips import all_tips
        self.assertIn("insufficient_data", build_profile(self.db, source="codex"))
        self.assertEqual(all_tips(self.db, source="codex"), [])


class MixedSourceTests(unittest.TestCase):
    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.db = str(self.tmp / "t.db")
        init_db(self.db)
        write_jsonl(self.tmp / "claude" / "-p" / "s.jsonl", [
            {"type": "assistant", "uuid": "a1", "sessionId": "cs1", "timestamp": "2026-04-10T00:00:01Z",
             "message": {"model": "claude-opus-4-7", "id": "m1", "content": [],
                         "usage": {"input_tokens": 10, "output_tokens": 5}}}])
        write_jsonl(self.tmp / "codex" / f"rollout-{SID}.jsonl", codex_lines())
        write_jsonl(self.tmp / "other.jsonl", [
            {"session_id": "g1", "timestamp": "2026-04-11T00:00:00Z", "model": "aider-x",
             "input_tokens": 100, "output_tokens": 40, "cache_read_tokens": 7, "cwd": "/a/b",
             "prompt": "hello", "tools": ["edit"]},
            {"timestamp": "missing session id is skipped"}])
        self.sources = [
            {"name": "claude", "kind": "claude", "path": str(self.tmp / "claude")},
            {"name": "codex", "kind": "codex", "path": str(self.tmp / "codex")},
            {"name": "aider", "kind": "generic", "path": str(self.tmp / "other.jsonl")},
        ]

    def test_sources_stay_separate_and_sum(self):
        scan_sources(self.sources, self.db)
        by = {r["source"]: r for r in source_breakdown(self.db)}
        self.assertEqual(set(by), {"claude", "codex", "aider"})
        self.assertEqual(by["claude"]["input_tokens"], 10)
        self.assertEqual(by["aider"]["input_tokens"], 100)
        self.assertEqual(by["aider"]["cache_read_tokens"], 7)
        self.assertEqual(by["aider"]["turns"], 1)
        total = overview_totals(self.db)
        self.assertEqual(total["output_tokens"], 5 + 80 + 40)
        self.assertEqual(overview_totals(self.db, source="claude")["output_tokens"], 5)

    def test_unknown_kind_is_an_error(self):
        with self.assertRaises(ValueError):
            scan_source("x", "nope", self.tmp, self.db)


class SourceConfigTests(unittest.TestCase):
    def test_parse(self):
        self.assertEqual(parse_source_arg("codex=codex:~/.codex/sessions"),
                         {"name": "codex", "kind": "codex", "path": "~/.codex/sessions"})
        self.assertEqual(parse_source_arg("codex=/some/dir")["kind"], "codex")  # bare path: name is the kind
        self.assertEqual(parse_source_arg("mybot=generic:C:\\logs\\a.jsonl")["path"], "C:\\logs\\a.jsonl")
        for bad in ("nonsense", "x=weird:/p", "=codex:/p"):
            with self.assertRaises(ValueError):
                parse_source_arg(bad)

    def test_resolve_order_override_and_pin(self):
        cfg = Path(tempfile.mkdtemp()) / "s.json"
        cfg.write_text(json.dumps([{"name": "codex", "kind": "codex", "path": "/cfg"}]))
        got = resolve_sources("/claude", extra=["codex=codex:/flag"], config_path=cfg)
        self.assertEqual([(s["name"], s["path"]) for s in got], [("claude", "/claude"), ("codex", "/flag")])
        pinned = resolve_sources("/claude", use_config=False, config_path=cfg)
        self.assertEqual([s["name"] for s in pinned], ["claude"])


if __name__ == "__main__":
    unittest.main()


class HermesTests(unittest.TestCase):
    def setUp(self):
        import sqlite3
        self.tmp = Path(tempfile.mkdtemp())
        self.db = str(self.tmp / "t.db")
        init_db(self.db)
        self.home = self.tmp / "hermes"
        (self.home / "profiles" / "builder").mkdir(parents=True)
        for path, model, i, o, cr in ((self.home / "state.db", "ojas-qwen", 1000, 100, 5000),
                                      (self.home / "profiles" / "builder" / "state.db", "claude-sonnet-5", 10, 50, 900)):
            c = sqlite3.connect(path)
            c.executescript("""
              CREATE TABLE sessions (id TEXT PRIMARY KEY, source TEXT, model TEXT, started_at REAL, ended_at REAL,
                message_count INT, input_tokens INT, output_tokens INT, cache_read_tokens INT, cache_write_tokens INT,
                api_call_count INT, cwd TEXT, git_branch TEXT);
              CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT, role TEXT, content TEXT,
                tool_calls TEXT, tool_name TEXT, tool_call_id TEXT, timestamp REAL);""")
            sid = "2026_s1_" + model[:4]
            c.execute("INSERT INTO sessions VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
                      (sid, "cli", model, 1.79e9, 1.79e9 + 10, 5, i, o, cr, 0, 3, "/Users/x/proj", "main"))
            call = [{"id": "c1", "function": {"name": "terminal", "arguments": json.dumps({"command": "ls"})}}]
            for n, (role, content, tc) in enumerate([("user", "hello there", None), ("assistant", None, json.dumps(call)),
                                                     ("tool", json.dumps({"exit_code": 1, "output": "x" * 80}), None),
                                                     ("assistant", "done", None), ("assistant", "bye", None)]):
                c.execute("INSERT INTO messages (session_id, role, content, tool_calls, tool_call_id, timestamp) VALUES (?,?,?,?,?,?)",
                          (sid, role, content, tc, "c1" if role == "tool" else None, 1.79e9 + n))
            c.commit()
            c.close()
        self.opts = {"model_alias": {"ojas-qwen": "qwen3.5"}}

    def test_totals_exact_alias_and_profiles(self):
        from token_dashboard.scanner import scan_hermes
        scan_hermes("hermes", self.home, self.db, self.opts)
        t = overview_totals(self.db, source="hermes")
        self.assertEqual((t["input_tokens"], t["output_tokens"], t["cache_read_tokens"]), (1010, 150, 5900))  # exact, incl. remainder split
        self.assertEqual(t["sessions"], 2)  # main DB + builder profile DB
        models = {m["model"]: m for m in model_breakdown(self.db, source="hermes")}
        self.assertEqual(set(models), {"qwen3.5", "claude-sonnet-5"})
        self.assertEqual(models["qwen3.5"]["turns"], 3)
        with connect(self.db) as c:
            self.assertEqual(c.execute("SELECT DISTINCT agent_id FROM messages WHERE agent_id IS NOT NULL").fetchone()[0], "builder")
            tools = {(r["tool_name"], r["is_error"]) for r in c.execute("SELECT * FROM tool_calls")}
        self.assertIn(("Bash", 0), tools)
        self.assertIn(("_tool_result", 1), tools)  # exit_code 1 -> error

    def test_rescan_unchanged_is_noop_and_changed_session_replaced(self):
        import sqlite3
        from token_dashboard.scanner import scan_hermes
        scan_hermes("hermes", self.home, self.db, self.opts)
        self.assertEqual(scan_hermes("hermes", self.home, self.db, self.opts)["messages"], 0)
        c = sqlite3.connect(self.home / "state.db")
        c.execute("UPDATE sessions SET output_tokens = 400, message_count = 6"); c.commit(); c.close()
        scan_hermes("hermes", self.home, self.db, self.opts)
        self.assertEqual(overview_totals(self.db, source="hermes")["output_tokens"], 450)  # replaced, not added
        with connect(self.db) as c:
            n = c.execute("SELECT COUNT(*) FROM messages WHERE type='user'").fetchone()[0]
        self.assertEqual(n, 2)  # no duplicate prompt rows

    def test_alias_change_reingests(self):
        from token_dashboard.scanner import scan_hermes
        scan_hermes("hermes", self.home, self.db, {})
        self.assertIn("ojas-qwen", {m["model"] for m in model_breakdown(self.db)})
        scan_hermes("hermes", self.home, self.db, self.opts)
        self.assertNotIn("ojas-qwen", {m["model"] for m in model_breakdown(self.db)})


class AgentPlanTests(unittest.TestCase):
    def test_agent_plan_falls_back_to_global(self):
        from token_dashboard.pricing import get_plan, set_plan, agent_plans
        db = str(Path(tempfile.mkdtemp()) / "t.db")
        init_db(db)
        self.assertEqual(get_plan(db, source="codex"), "api")
        set_plan(db, "pro")
        set_plan(db, "max", "codex")
        self.assertEqual((get_plan(db), get_plan(db, source="codex"), get_plan(db, source="claude")), ("pro", "max", "pro"))
        self.assertEqual(agent_plans(db), {"codex": "max"})
