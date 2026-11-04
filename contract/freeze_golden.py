#!/usr/bin/env python3
"""Freeze the Python server's responses over contract/fixtures into contract/golden/*.json.

    python3 contract/freeze_golden.py   # only to re-freeze after an intended Python change

The Go port must reproduce these (see go test ./go/internal/contract, or point
CONTRACT_BASE_URL at any running server that was started over the same fixtures).
"""
import http.client
import http.server
import json
import os
import shutil
import sqlite3
import sys
import tempfile
import threading
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent / "python"
FIX = HERE / "fixtures"
GOLD = HERE / "golden"
FIXED_TODAY = "2026-09-20T12:00:00"

os.environ["TOKEN_DASHBOARD_SKILL_ROOTS"] = str(FIX / "skills")
sys.path.insert(0, str(ROOT))

from token_dashboard.db import init_db  # noqa: E402
from token_dashboard.pricing import set_plan  # noqa: E402
from token_dashboard.scanner import scan_sources  # noqa: E402
from token_dashboard.server import build_handler  # noqa: E402
from token_dashboard.tips import all_tips, dismiss_tip  # noqa: E402


def fixture_sources():
    out = []
    for e in json.loads((HERE / "sources.fixture.json").read_text()):
        s = {"name": e["name"], "kind": e["kind"], "path": str(FIX / e["path"])}
        if e.get("model_alias"):
            s["options"] = {"model_alias": e["model_alias"]}
        out.append(s)
    return out


R = "since=2026-09-15T00:00:00Z&until=2026-09-18T00:00:00Z"
REQUESTS = [
    ("overview", "GET", "/api/overview"),
    ("overview_range", "GET", f"/api/overview?{R}"),
    ("overview_codex", "GET", "/api/overview?source=codex"),
    ("overview_hermes", "GET", "/api/overview?source=hermes"),
    ("prompts", "GET", "/api/prompts"),
    ("prompts_recent5", "GET", "/api/prompts?limit=5&sort=recent"),
    ("prompts_limit0", "GET", "/api/prompts?limit=0"),
    ("prompts_limit_huge", "GET", "/api/prompts?limit=99999"),
    ("prompts_claude", "GET", "/api/prompts?source=claude"),
    ("projects", "GET", "/api/projects"),
    ("projects_range", "GET", f"/api/projects?{R}"),
    ("profile", "GET", "/api/profile"),
    ("profile_claude", "GET", "/api/profile?source=claude"),
    ("profile_codex", "GET", "/api/profile?source=codex"),
    ("tools", "GET", "/api/tools"),
    ("tools_claude", "GET", "/api/tools?source=claude"),
    ("sessions", "GET", "/api/sessions"),
    ("sessions_limit3", "GET", "/api/sessions?limit=3"),
    ("sessions_range", "GET", f"/api/sessions?{R}"),
    ("daily", "GET", "/api/daily"),
    ("daily_hermes", "GET", "/api/daily?source=hermes"),
    ("skills", "GET", "/api/skills"),
    ("skills_claude", "GET", "/api/skills?source=claude"),
    ("by_source", "GET", "/api/by-source"),
    ("by_model", "GET", "/api/by-model"),
    ("by_model_codex", "GET", "/api/by-model?source=codex"),
    ("session_alpha", "GET", "/api/sessions/sess-alpha-1"),
    ("session_beta", "GET", "/api/sessions/sess-beta-1"),
    ("session_missing", "GET", "/api/sessions/nope"),
    ("session_codex", "GET", "/api/sessions/01a03549-440a-7b53-b769-16c8a6a19ba0"),
    ("session_hermes", "GET", "/api/sessions/h_s1_ojas"),
    ("tips", "GET", "/api/tips"),
    ("tips_codex", "GET", "/api/tips?source=codex"),
    ("recommendations", "GET", "/api/recommendations"),
    ("plan", "GET", "/api/plan"),
    ("scan_noop", "GET", "/api/scan"),
    ("readyz", "GET", "/readyz"),
    ("err_404", "GET", "/api/nope"),
    ("err_host", "GET", "/api/overview", {"Host": "evil.example.com"}),
    ("err_post_ctype", "POST", "/api/plan", {"Content-Type": "text/plain"}, "plan=pro"),
    ("err_post_badjson", "POST", "/api/plan", {"Content-Type": "application/json"}, "{nope"),
    ("err_post_notobj", "POST", "/api/plan", {"Content-Type": "application/json"}, "[1]"),
    ("err_post_404", "POST", "/api/other", {"Content-Type": "application/json"}, "{}"),
]


def main():
    tmp = Path(tempfile.mkdtemp())
    db = str(tmp / "contract.db")
    init_db(db)
    srcs = fixture_sources()
    scan_sources(srcs, db)
    set_plan(db, "pro", "codex")
    dismiss_tip(db, "claude:repeat-bash:make test")  # a dismissed tip must be filtered out
    H = build_handler(db, projects_dir=str(FIX / "claude"), sources=srcs)
    httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    port = httpd.server_address[1]

    if GOLD.exists():
        shutil.rmtree(GOLD)
    GOLD.mkdir()
    for name, method, path, *rest in REQUESTS:
        headers = rest[0] if rest else {}
        body = rest[1] if len(rest) > 1 else None
        conn = http.client.HTTPConnection("127.0.0.1", port)
        conn.request(method, path, body=body, headers=headers)
        resp = conn.getresponse()
        raw = resp.read()
        conn.close()
        try:
            parsed = json.loads(raw) if raw else None
        except ValueError:
            parsed = {"__raw__": raw.decode()}
        g = {"name": name, "request": {"method": method, "path": path, "headers": headers, "body": body},
             "status": resp.status, "body": parsed}
        (GOLD / f"{name}.json").write_text(json.dumps(g, indent=1, sort_keys=True) + "\n")

    # Tips with a pinned clock: the HTTP endpoint uses "now", which would make goldens drift.
    (GOLD / "_tips_fixed_clock.json").write_text(json.dumps(
        {"today_iso": FIXED_TODAY, "all": all_tips(db, FIXED_TODAY), "codex": all_tips(db, FIXED_TODAY, "codex"),
         "claude": all_tips(db, FIXED_TODAY, "claude")}, indent=1, sort_keys=True) + "\n")

    # Differential summary straight from the DB: row counts + per-session token totals.
    c = sqlite3.connect(db)
    c.row_factory = sqlite3.Row
    summary = {
        "messages_by_source": {r[0]: r[1] for r in c.execute("SELECT source, COUNT(*) FROM messages GROUP BY source")},
        "tool_calls_by_source": {r[0]: r[1] for r in c.execute("SELECT source, COUNT(*) FROM tool_calls GROUP BY source")},
        "tool_calls_by_name": {r[0]: r[1] for r in c.execute("SELECT tool_name, COUNT(*) FROM tool_calls GROUP BY tool_name")},
        "files": c.execute("SELECT COUNT(*) FROM files").fetchone()[0],
        "sessions": {r["session_id"]: dict(r) for r in c.execute(
            "SELECT session_id, source, COUNT(*) msgs, SUM(input_tokens) i, SUM(output_tokens) o, SUM(cache_read_tokens) cr,"
            " SUM(cache_create_5m_tokens) c5, SUM(cache_create_1h_tokens) c1 FROM messages GROUP BY session_id, source")},
        "message_uuids": [r[0] for r in c.execute("SELECT uuid FROM messages ORDER BY uuid")],
    }
    (GOLD / "_db_summary.json").write_text(json.dumps(summary, indent=1, sort_keys=True) + "\n")
    print(f"froze {len(REQUESTS)} responses + tips clock + db summary into {GOLD}")
    httpd.shutdown()
    shutil.rmtree(tmp, ignore_errors=True)


if __name__ == "__main__":
    main()
