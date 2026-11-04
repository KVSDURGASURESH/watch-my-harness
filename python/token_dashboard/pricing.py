"""Pricing table + plan-aware cost formatting."""
from __future__ import annotations

import json
from pathlib import Path
from typing import Optional, Union

from .db import connect


def load_pricing(path: Union[str, Path]) -> dict:
    return json.loads(Path(path).read_text(encoding="utf-8"))


def _tier_from_name(model: str) -> Optional[str]:
    m = (model or "").lower()
    for tier in ("opus", "sonnet", "haiku"):
        if tier in m:
            return tier
    return None


def cost_for(model: str, usage: dict, pricing: dict) -> dict:
    """Return {usd, estimated, breakdown}. usd=None when no tier match."""
    rates = pricing["models"].get(model)
    estimated = False
    if rates is None:
        tier = _tier_from_name(model or "")
        if tier and tier in pricing["tier_fallback"]:
            rates = pricing["tier_fallback"][tier]
            estimated = True
        else:
            return {"usd": None, "estimated": True, "breakdown": {}}
    bd = {
        "input":           usage["input_tokens"]            * rates["input"]           / 1_000_000,
        "output":          usage["output_tokens"]           * rates["output"]          / 1_000_000,
        "cache_read":      usage["cache_read_tokens"]       * rates.get("cache_read", 0)      / 1_000_000,
        "cache_create_5m": usage["cache_create_5m_tokens"]  * rates.get("cache_create_5m", 0) / 1_000_000,
        "cache_create_1h": usage["cache_create_1h_tokens"]  * rates.get("cache_create_1h", 0) / 1_000_000,
    }
    return {"usd": round(sum(bd.values()), 6), "estimated": estimated, "breakdown": bd}


def _plan_key(source: Optional[str]) -> str:
    return f"plan:{source}" if source else "plan"


def get_plan(db_path: Union[str, Path], default: str = "api", source: Optional[str] = None) -> str:
    """Plan for one agent, falling back to the global plan, then `default`."""
    with connect(db_path) as c:
        for k in ([_plan_key(source)] if source else []) + ["plan"]:
            row = c.execute("SELECT v FROM plan WHERE k=?", (k,)).fetchone()
            if row:
                return row["v"]
    return default


def agent_plans(db_path: Union[str, Path]) -> dict:
    """{agent: plan} for agents that have their own plan set."""
    with connect(db_path) as c:
        return {r["k"][5:]: r["v"] for r in c.execute("SELECT k, v FROM plan WHERE k LIKE 'plan:%'")}


def set_plan(db_path: Union[str, Path], plan: str, source: Optional[str] = None) -> None:
    with connect(db_path) as c:
        c.execute("INSERT OR REPLACE INTO plan (k, v) VALUES (?, ?)", (_plan_key(source), plan))
        c.commit()


def format_for_user(api_cost_usd: float, plan: str, pricing: dict) -> dict:
    p = pricing["plans"].get(plan, pricing["plans"]["api"])
    if plan == "api" or p["monthly"] == 0:
        return {"display_usd": api_cost_usd, "subtitle": None, "subscription_usd": None}
    return {
        "display_usd":      api_cost_usd,
        "subtitle":         f"You pay ${p['monthly']}/mo on {p['label']}",
        "subscription_usd": p["monthly"],
    }
