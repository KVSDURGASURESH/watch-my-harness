"""Draft CLAUDE.md-style recommendations, generated from the existing
rule-based Tips engine's findings.

This intentionally reuses tips.py rather than inventing new detection
logic: those findings are already concrete and evidence-based (a file
read N times, a measured cache-hit rate), which is the whole point --
a narrated summary of aggregate scores would just be nicer-sounding
hype around the same thin signal. Never writes to ~/.claude/CLAUDE.md
itself -- produces a draft for the user to review and merge manually.
"""
from __future__ import annotations

from typing import Optional

from .tips import all_tips

HEADER = "## From TOKEN DASHBOARD — draft harness recommendations"

INTRO = (
    "Generated from rule-based patterns in your own agent session "
    "history. Review before merging into ~/.claude/CLAUDE.md (or the matching agent's AGENTS.md) — nothing "
    "here is applied automatically."
)


def build_recommendations_markdown(db_path, today_iso: Optional[str] = None, source=None) -> str:
    tips = all_tips(db_path, today_iso, source)
    if not tips:
        return ""
    lines = [HEADER, "", INTRO, ""]
    for tip in tips:
        agent = f" _({tip['source']})_" if tip.get("source") else ""
        lines.append(f"- **{tip['title']}**{agent}")
        lines.append(f"  {tip['body']}")
        lines.append("")
    return "\n".join(lines).rstrip() + "\n"
