"""Local, rule-based "Builder Profile" — session-history scoring across four
dimensions. Every metric is derived from data already in this project's
SQLite cache; nothing is sent anywhere. Scores are heuristic, fixed-threshold
bands over the user's own history, not a comparison to other people — there
is no population of other users to rank against locally.
"""
from __future__ import annotations

from .db import connect, _range_clause

MIN_USER_TURNS_FOR_PROFILE = 10

DIMENSIONS = ("steering", "execution", "engineering", "planning")

PLANNING_TOOLS = ("Task", "Skill", "TodoWrite")

ARCHETYPES = {
    frozenset({"engineering", "planning"}): "The Architect",
    frozenset({"steering", "engineering"}): "The Precision Editor",
    frozenset({"execution", "steering"}): "The Sprinter",
    frozenset({"execution", "planning"}): "The Operator",
    frozenset({"execution", "engineering"}): "The Craftsman",
    frozenset({"steering", "planning"}): "The Strategist",
}

BALANCE_THRESHOLD = 10


def _clamp_score(value: float, worst: float, best: float) -> float:
    """Map value onto 0-100, where `worst` -> 0 and `best` -> 100 (either
    direction), clamped outside the range."""
    if worst == best:
        return 100.0
    frac = (value - worst) / (best - worst)
    return max(0.0, min(1.0, frac)) * 100.0


def steering_score(db_path, since=None, until=None, source=None) -> int:
    """How specific and decisive prompting is: substantial (non-trivial)
    prompts, and sessions that converge without excessive back-and-forth."""
    rng, args = _range_clause(since, until, source=source)
    with connect(db_path) as c:
        prompt_row = c.execute(f"""
            SELECT COUNT(*) AS n,
                   SUM(CASE WHEN prompt_chars >= 40 THEN 1 ELSE 0 END) AS substantial
              FROM messages
             WHERE type='user' AND prompt_chars IS NOT NULL {rng}
        """, args).fetchone()
        turns_row = c.execute(f"""
            SELECT AVG(turns) AS avg_turns FROM (
                SELECT session_id, SUM(CASE WHEN type='user' THEN 1 ELSE 0 END) AS turns
                  FROM messages WHERE 1=1 {rng}
                 GROUP BY session_id
                HAVING turns > 0
            )
        """, args).fetchone()
    n = prompt_row["n"] or 0
    substantial_pct = (prompt_row["substantial"] or 0) / n if n else 0.0
    prompt_score = _clamp_score(substantial_pct, worst=0.0, best=0.8)
    avg_turns = turns_row["avg_turns"] or 0.0
    turns_score = _clamp_score(avg_turns, worst=30.0, best=8.0)
    return round((prompt_score + turns_score) / 2)


def execution_score(db_path, since=None, until=None, source=None) -> int:
    """Clean, capable tool use: low error rate, broad tool fluency."""
    rng, args = _range_clause(since, until, source=source, tool_table=True)
    with connect(db_path) as c:
        n = c.execute(f"""
            SELECT COUNT(*) AS n
              FROM tool_calls WHERE tool_name != '_tool_result' {rng}
        """, args).fetchone()["n"] or 0
        errors = c.execute(f"""
            SELECT COUNT(*) AS n
              FROM tool_calls WHERE tool_name = '_tool_result' AND is_error = 1 {rng}
        """, args).fetchone()["n"] or 0
        diversity = c.execute(f"""
            SELECT COUNT(DISTINCT tool_name) AS n
              FROM tool_calls WHERE tool_name != '_tool_result' {rng}
        """, args).fetchone()["n"] or 0
    error_rate = errors / n if n else 0.0
    error_score = _clamp_score(error_rate, worst=0.10, best=0.0)
    diversity_score = _clamp_score(diversity, worst=1, best=8)
    return round((error_score + diversity_score) / 2)


def engineering_score(db_path, since=None, until=None, source=None) -> int:
    """Healthy editing discipline: low rework (re-touching the same file
    over and over within a session) and reasonable breadth."""
    rng, args = _range_clause(since, until, source=source, tool_table=True)
    with connect(db_path) as c:
        # One row per (session, file) touched, plus how many of those were
        # touched more than twice in the same session.
        retouch_row = c.execute(f"""
            SELECT COUNT(*) AS files,
                   SUM(CASE WHEN touches > 2 THEN 1 ELSE 0 END) AS retouched
              FROM (
                SELECT session_id, target, COUNT(*) AS touches
                  FROM tool_calls
                 WHERE tool_name IN ('Read','Edit','Write') AND target IS NOT NULL {rng}
                 GROUP BY session_id, target
              )
        """, args).fetchone()
        breadth_row = c.execute(f"""
            SELECT AVG(files) AS avg_files FROM (
                SELECT session_id, COUNT(DISTINCT target) AS files
                  FROM tool_calls
                 WHERE tool_name IN ('Read','Edit','Write') AND target IS NOT NULL {rng}
                 GROUP BY session_id
            )
        """, args).fetchone()
    total_files = retouch_row["files"] or 0
    retouch_rate = (retouch_row["retouched"] or 0) / total_files if total_files else 0.0
    retouch_score = _clamp_score(retouch_rate, worst=0.5, best=0.0)
    avg_files = breadth_row["avg_files"] or 0.0
    breadth_score = _clamp_score(avg_files, worst=1.0, best=10.0)
    return round((retouch_score + breadth_score) / 2)


def planning_score(db_path, since=None, until=None, source=None) -> int:
    """Share of sessions that use structured delegation (subagents, skills,
    or todo tracking) rather than freeform back-and-forth."""
    rng, args = _range_clause(since, until, source=source)
    trng, targs = _range_clause(since, until, source=source, tool_table=True)
    placeholders = ",".join("?" for _ in PLANNING_TOOLS)
    with connect(db_path) as c:
        total = c.execute(
            f"SELECT COUNT(DISTINCT session_id) AS n FROM messages WHERE 1=1 {rng}", args
        ).fetchone()["n"] or 0
        planned = c.execute(
            f"SELECT COUNT(DISTINCT session_id) AS n FROM tool_calls "
            f"WHERE tool_name IN ({placeholders}) {trng}",
            list(PLANNING_TOOLS) + targs,
        ).fetchone()["n"] or 0
    pct = planned / total if total else 0.0
    return round(_clamp_score(pct, worst=0.0, best=0.5))


def _archetype(scores: dict) -> str:
    """Pick the archetype label. "Generalist" means all four dimensions are
    roughly level (max-min spread within BALANCE_THRESHOLD) — no clear
    specialty. Otherwise, the two highest-scoring dimensions pick the label
    (tie-broken alphabetically for determinism)."""
    spread = max(scores.values()) - min(scores.values())
    if spread <= BALANCE_THRESHOLD:
        return "The Generalist"
    ranked = sorted(DIMENSIONS, key=lambda d: (-scores[d], d))
    top, second = ranked[0], ranked[1]
    return ARCHETYPES.get(frozenset({top, second}), "The Generalist")


def build_profile(db_path, since=None, until=None, source=None) -> dict:
    rng, args = _range_clause(since, until, source=source)
    with connect(db_path) as c:
        turns = c.execute(
            f"SELECT COUNT(*) AS n FROM messages WHERE type='user' {rng}", args
        ).fetchone()["n"] or 0
    if turns < MIN_USER_TURNS_FOR_PROFILE:
        return {
            "insufficient_data": True,
            "turns": turns,
            "minimum": MIN_USER_TURNS_FOR_PROFILE,
        }
    scores = {
        "steering": steering_score(db_path, since, until, source),
        "execution": execution_score(db_path, since, until, source),
        "engineering": engineering_score(db_path, since, until, source),
        "planning": planning_score(db_path, since, until, source),
    }
    return {
        "insufficient_data": False,
        "turns": turns,
        "scores": scores,
        "archetype": _archetype(scores),
    }
