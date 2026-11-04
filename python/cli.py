"""Token Dashboard CLI entrypoint."""
from __future__ import annotations

import argparse
import os
import shutil
import subprocess
import threading
import urllib.request
import webbrowser
from datetime import datetime, timedelta, timezone
from pathlib import Path

from token_dashboard.db import init_db, default_db_path, overview_totals
from token_dashboard.recommendations import build_recommendations_markdown
from token_dashboard.scanner import scan_sources
from token_dashboard.sources import Manager, resolve_sources, CODEX_DEFAULT
from token_dashboard.tips import all_tips


def _db_path(args) -> str:
    return getattr(args, "db", None) or os.environ.get("TOKEN_DASHBOARD_DB") or str(default_db_path())


def _projects(args) -> str:
    return (
        getattr(args, "projects_dir", None)
        or os.environ.get("CLAUDE_PROJECTS_DIR")
        or str(Path.home() / ".claude" / "projects")
    )


def _sources(args) -> list:
    """Claude Code + any configured/flagged agents. An explicit --projects-dir
    skips the config file so it means exactly that directory."""
    extra = list(getattr(args, "source", None) or [])
    if getattr(args, "codex", False):
        extra.insert(0, f"codex=codex:{CODEX_DEFAULT}")
    pinned = bool(getattr(args, "projects_dir", None) or os.environ.get("CLAUDE_PROJECTS_DIR"))
    try:
        return resolve_sources(_projects(args), use_config=not pinned, extra=extra)
    except ValueError as e:
        raise SystemExit(f"Token Dashboard: {e}")


def _today_range():
    now = datetime.now(timezone.utc)
    start = datetime(now.year, now.month, now.day, tzinfo=timezone.utc).isoformat()
    end = (now + timedelta(days=1)).replace(hour=0, minute=0, second=0, microsecond=0).isoformat()
    return start, end


def cmd_scan(args):
    db = _db_path(args)
    init_db(db)
    n = scan_sources(_sources(args), db)
    print(f"Token Dashboard: scanned {n['files']} files, {n['messages']} messages, {n['tools']} tool calls")


def cmd_today(args):
    db = _db_path(args)
    init_db(db)
    s, e = _today_range()
    t = overview_totals(db, since=s, until=e)
    print("Token Dashboard — today")
    print(f"  sessions: {t['sessions']}    turns: {t['turns']}")
    print(f"  input:    {t['input_tokens']:>12,}    output: {t['output_tokens']:>12,}")
    print(f"  cache rd: {t['cache_read_tokens']:>12,}    cache cr: {t['cache_create_5m_tokens']+t['cache_create_1h_tokens']:>12,}")


def cmd_stats(args):
    db = _db_path(args)
    init_db(db)
    t = overview_totals(db)
    print("Token Dashboard — all time")
    print(f"  sessions: {t['sessions']}    turns: {t['turns']}")
    print(f"  input:    {t['input_tokens']:>12,}    output: {t['output_tokens']:>12,}")


def cmd_tips(args):
    db = _db_path(args)
    init_db(db)
    tips = all_tips(db)
    if not tips:
        print("Token Dashboard: no suggestions")
        return
    for tip in tips:
        print(f"[{tip['category']}] {tip['title']}")
        print(f"  {tip['body']}\n")


def cmd_recommend(args):
    db = _db_path(args)
    init_db(db)
    md = build_recommendations_markdown(db)
    if not md:
        print("Token Dashboard: no recommendations right now — nothing crossed a threshold.")
        return
    print(md)
    print("Draft only — review it, then merge whatever you agree with into ~/.claude/CLAUDE.md yourself. Nothing was written automatically.")


def _is_running(url: str) -> bool:
    try:
        with urllib.request.urlopen(url + "healthz", timeout=1.5):
            return True
    except Exception:
        return False


def _find_chromium() -> str | None:
    cands = [
        "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
        "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
        "/Applications/Chromium.app/Contents/MacOS/Chromium",
        r"C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
        r"C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe",
    ]
    for c in cands:
        if os.path.exists(c):
            return c
    for n in ("google-chrome", "chromium", "chromium-browser", "microsoft-edge"):
        w = shutil.which(n)
        if w:
            return w
    return None


def _open_app_window(url: str) -> None:
    """Chromeless app window with its own profile (own dock/taskbar entry); falls back to the default browser."""
    exe = _find_chromium()
    if not exe:
        webbrowser.open(url)
        return
    profile = os.path.expanduser("~/.claude/token-dashboard-app")
    subprocess.Popen([exe, f"--app={url}", f"--user-data-dir={profile}", "--window-size=1440,900"],
                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def _extra(args) -> list:
    extra = list(getattr(args, "source", None) or [])
    if getattr(args, "codex", False):
        extra.insert(0, f"codex=codex:{CODEX_DEFAULT}")
    return extra


def _manager(args) -> Manager:
    """Live source resolver for the dashboard, so Settings > Sources edits apply without a restart."""
    pinned = bool(getattr(args, "projects_dir", None) or os.environ.get("CLAUDE_PROJECTS_DIR"))
    return Manager(_projects(args), pinned=pinned, extra=_extra(args))


def cmd_dashboard(args):
    db = _db_path(args)
    init_db(db)
    from token_dashboard.server import run

    host = os.environ.get("HOST", "127.0.0.1")
    port = int(os.environ.get("PORT", "8191"))
    url = f"http://{host}:{port}/"
    if getattr(args, "app", False):
        # Single instance: if a dashboard already answers, just open a window to it.
        if _is_running(url):
            print(f"Token Dashboard already running on {url}")
            _open_app_window(url)
            return
        threading.Timer(1.0, _open_app_window, args=(url,)).start()
    elif not args.no_open:
        webbrowser.open(url)
    print(f"Token Dashboard listening on {url}")
    # Scanning happens on a background thread inside run() so the server
    # starts accepting connections immediately, instead of blocking behind
    # a full scan of potentially years of session history. The frontend
    # renders progressively as scan results arrive via the SSE stream.
    # First run with no explicit source flags shows the setup screen before scanning.
    explicit = bool(getattr(args, "projects_dir", None) or os.environ.get("CLAUDE_PROJECTS_DIR") or _extra(args))
    gate = not explicit and os.environ.get("TOKEN_DASHBOARD_NO_SETUP") != "1"
    try:
        mgr = _manager(args)
        mgr.effective()  # fail fast on a bad sources file or --source flag
    except ValueError as e:
        raise SystemExit(f"Token Dashboard: {e}")
    run(host, port, db, _projects(args), scan=not args.no_scan, manager=mgr, require_setup=gate)


def main():
    # default=SUPPRESS on both: if --db/--projects-dir aren't given at a
    # given parser level, that level leaves the attribute alone entirely
    # instead of resetting it to None — otherwise a subparser inheriting
    # these via `parents=[common]` silently clobbers a value already set
    # by the top-level parser (e.g. `token-dashboard --db X dashboard`
    # would lose X). _db_path/_projects use getattr(..., None) for the
    # case where neither level ever set the attribute at all.
    common = argparse.ArgumentParser(add_help=False)
    common.add_argument("--db", default=argparse.SUPPRESS, help="SQLite path (default ~/.claude/token-dashboard.db)")
    common.add_argument("--projects-dir", default=argparse.SUPPRESS, help="JSONL root (default ~/.claude/projects)")

    common.add_argument("--source", action="append", default=argparse.SUPPRESS, metavar="NAME=KIND:PATH",
                        help="extra agent log source, repeatable; kinds: claude, codex, generic "
                             "(e.g. codex=codex:~/.codex/sessions)")
    common.add_argument("--codex", action="store_true", default=argparse.SUPPRESS,
                        help="shorthand for --source codex=codex:~/.codex/sessions")

    p = argparse.ArgumentParser(prog="token-dashboard", description="Local Claude Code usage dashboard", parents=[common])
    sub = p.add_subparsers(dest="cmd", required=True)
    sub.add_parser("scan",  parents=[common]).set_defaults(func=cmd_scan)
    sub.add_parser("today", parents=[common]).set_defaults(func=cmd_today)
    sub.add_parser("stats", parents=[common]).set_defaults(func=cmd_stats)
    sub.add_parser("tips",  parents=[common]).set_defaults(func=cmd_tips)
    sub.add_parser("recommend", parents=[common]).set_defaults(func=cmd_recommend)
    d = sub.add_parser("dashboard", parents=[common])
    d.add_argument("--no-scan", action="store_true")
    d.add_argument("--no-open", action="store_true")
    d.add_argument("--app", action="store_true", help="open in a chromeless app window (reuses a running instance)")
    d.set_defaults(func=cmd_dashboard)
    args = p.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
