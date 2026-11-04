import json
import os
import tempfile
import unittest

from token_dashboard.db import init_db, connect
from token_dashboard.profile import (
    build_profile, steering_score, execution_score,
    engineering_score, planning_score, MIN_USER_TURNS_FOR_PROFILE, _archetype,
)
from token_dashboard.scanner import scan_dir


class ProfileTestBase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.db = os.path.join(self.tmp, "t.db")
        init_db(self.db)

    def _msg(self, uuid, session, mtype, ts, prompt_chars=None):
        with connect(self.db) as c:
            c.execute(
                "INSERT INTO messages (uuid, session_id, project_slug, type, timestamp, prompt_chars) "
                "VALUES (?, ?, 'p', ?, ?, ?)",
                (uuid, session, mtype, ts, prompt_chars),
            )
            c.commit()

    def _tool(self, uuid_prefix, session, tool_name, target, ts, is_error=0):
        with connect(self.db) as c:
            c.execute(
                "INSERT INTO tool_calls (message_uuid, session_id, project_slug, tool_name, target, timestamp, is_error) "
                "VALUES (?, ?, 'p', ?, ?, ?, ?)",
                (f"{uuid_prefix}-tc", session, tool_name, target, ts, is_error),
            )
            c.commit()


class InsufficientDataTests(ProfileTestBase):
    def test_below_minimum_returns_insufficient_data(self):
        for i in range(MIN_USER_TURNS_FOR_PROFILE - 1):
            self._msg(f"u{i}", "s1", "user", "2026-04-15T00:00:00Z", prompt_chars=50)
        result = build_profile(self.db)
        self.assertTrue(result["insufficient_data"])
        self.assertEqual(result["turns"], MIN_USER_TURNS_FOR_PROFILE - 1)

    def test_at_minimum_returns_full_profile(self):
        for i in range(MIN_USER_TURNS_FOR_PROFILE):
            self._msg(f"u{i}", "s1", "user", "2026-04-15T00:00:00Z", prompt_chars=50)
        result = build_profile(self.db)
        self.assertFalse(result["insufficient_data"])
        self.assertIn("scores", result)
        self.assertIn("archetype", result)
        for dim in ("steering", "execution", "engineering", "planning"):
            self.assertIn(dim, result["scores"])


class SteeringScoreTests(ProfileTestBase):
    def test_substantial_prompts_and_few_turns_score_high(self):
        for i in range(10):
            self._msg(f"u{i}", "s1", "user", "2026-04-15T00:00:00Z", prompt_chars=300)
        score = steering_score(self.db)
        self.assertGreater(score, 70)

    def test_trivial_prompts_and_many_turns_score_low(self):
        for i in range(40):
            self._msg(f"u{i}", "s1", "user", "2026-04-15T00:00:00Z", prompt_chars=5)
        score = steering_score(self.db)
        self.assertLess(score, 30)


class ExecutionScoreTests(ProfileTestBase):
    def test_clean_diverse_tool_use_scores_high(self):
        self._msg("u1", "s1", "user", "2026-04-15T00:00:00Z")
        for i, tool in enumerate(["Read", "Edit", "Write", "Bash", "Grep", "Glob", "Task", "Skill"]):
            self._tool(f"t{i}", "s1", tool, "x", "2026-04-15T00:00:00Z", is_error=0)
        score = execution_score(self.db)
        self.assertGreater(score, 70)

    def test_high_error_rate_scores_low(self):
        self._msg("u1", "s1", "user", "2026-04-15T00:00:00Z")
        for i in range(20):
            self._tool(f"call{i}", "s1", "Bash", "x", "2026-04-15T00:00:00Z", is_error=0)
            self._tool(f"result{i}", "s1", "_tool_result", "toolu_x", "2026-04-15T00:00:00Z", is_error=1)
        score = execution_score(self.db)
        self.assertLess(score, 30)


class ExecutionScoreRealScannerTests(unittest.TestCase):
    """Anchors execution_score to the actual scanner pipeline, not hand-written
    tool_calls rows — the scanner is the only source of truth for what DB
    states are actually reachable (see the note on ExecutionScoreTests above)."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.db = os.path.join(self.tmp, "t.db")
        self.proj_root = os.path.join(self.tmp, "projects")
        os.makedirs(os.path.join(self.proj_root, "p"))
        init_db(self.db)

    def _write_jsonl(self, lines):
        path = os.path.join(self.proj_root, "p", "s1.jsonl")
        with open(path, "w", encoding="utf-8") as f:
            for line in lines:
                f.write(json.dumps(line) + "\n")

    def test_scanner_produced_errors_lower_execution_score(self):
        lines = []
        for i in range(15):
            lines.append({
                "type": "assistant", "uuid": f"a{i}", "sessionId": "s1",
                "timestamp": "2026-04-15T00:00:00Z", "isSidechain": False,
                "message": {
                    "model": "claude-sonnet-4-6",
                    "usage": {"input_tokens": 1, "output_tokens": 1},
                    "content": [
                        {"type": "tool_use", "id": f"tu{i}", "name": "Bash",
                         "input": {"command": "false"}},
                    ],
                },
            })
            lines.append({
                "type": "user", "uuid": f"u{i}", "sessionId": "s1",
                "timestamp": "2026-04-15T00:00:01Z", "isSidechain": False,
                "message": {"role": "user", "content": [
                    {"type": "tool_result", "tool_use_id": f"tu{i}",
                     "content": "command failed", "is_error": True},
                ]},
            })
        self._write_jsonl(lines)
        n = scan_dir(self.proj_root, self.db)
        self.assertEqual(n["tools"], 30)  # 15 tool_use + 15 tool_result
        score = execution_score(self.db)
        self.assertLess(score, 30, "execution_score must reflect real scanner-produced errors")


class EngineeringScoreTests(ProfileTestBase):
    def test_broad_low_retouch_scores_high(self):
        self._msg("u1", "s1", "user", "2026-04-15T00:00:00Z")
        for i in range(10):
            self._tool(f"t{i}", "s1", "Edit", f"file{i}.py", "2026-04-15T00:00:00Z")
        score = engineering_score(self.db)
        self.assertGreater(score, 60)

    def test_heavy_retouch_scores_low(self):
        self._msg("u1", "s1", "user", "2026-04-15T00:00:00Z")
        for i in range(10):
            self._tool(f"t{i}", "s1", "Edit", "same_file.py", "2026-04-15T00:00:00Z")
        score = engineering_score(self.db)
        self.assertLess(score, 40)


class PlanningScoreTests(ProfileTestBase):
    def test_frequent_delegation_scores_high(self):
        for i in range(4):
            self._msg(f"u{i}", f"s{i}", "user", "2026-04-15T00:00:00Z")
            self._tool(f"t{i}", f"s{i}", "Task", "subagent", "2026-04-15T00:00:00Z")
        score = planning_score(self.db)
        self.assertGreater(score, 90)

    def test_no_delegation_scores_zero(self):
        for i in range(4):
            self._msg(f"u{i}", f"s{i}", "user", "2026-04-15T00:00:00Z")
        score = planning_score(self.db)
        self.assertEqual(score, 0)


class ArchetypeLabelTests(unittest.TestCase):
    """_archetype() is pure label-selection logic — tested directly against
    synthetic score dicts rather than through the full scoring pipeline, so
    the test doesn't depend on the exact arithmetic of the four scoring
    functions (that arithmetic is already covered by the dimension tests
    above)."""

    def test_engineering_and_planning_top_gives_architect(self):
        scores = {"steering": 20, "execution": 30, "engineering": 90, "planning": 85}
        self.assertEqual(_archetype(scores), "The Architect")

    def test_all_scores_close_gives_generalist(self):
        scores = {"steering": 60, "execution": 62, "engineering": 58, "planning": 61}
        self.assertEqual(_archetype(scores), "The Generalist")

    def test_steering_and_planning_top_gives_strategist(self):
        scores = {"steering": 90, "planning": 88, "engineering": 25, "execution": 20}
        self.assertEqual(_archetype(scores), "The Strategist")

    def test_execution_and_engineering_top_gives_craftsman(self):
        scores = {"execution": 85, "engineering": 82, "steering": 30, "planning": 25}
        self.assertEqual(_archetype(scores), "The Craftsman")

    def test_three_way_tie_breaks_alphabetically(self):
        # engineering, planning, steering are tied at the top; execution trails.
        # Alphabetical tie-break must deterministically pick the same two
        # dimensions (engineering, planning — alphabetically first of the
        # three) every time, not depend on dict/set iteration order.
        scores = {"engineering": 90, "planning": 90, "steering": 90, "execution": 20}
        self.assertEqual(_archetype(scores), "The Architect")


if __name__ == "__main__":
    unittest.main()
