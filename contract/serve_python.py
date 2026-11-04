#!/usr/bin/env python3
"""Serve the fixtures with the Python server, for `CONTRACT_BASE_URL=... go test ./go/internal/contract`.

    python3 contract/serve_python.py 8199
"""
import http.server, os, sys, tempfile
from pathlib import Path
HERE = Path(__file__).resolve().parent
os.environ["TOKEN_DASHBOARD_SKILL_ROOTS"] = str(HERE / "fixtures" / "skills")
sys.path.insert(0, str(HERE.parent / "python"))
sys.path.insert(0, str(HERE))
import freeze_golden as fg  # noqa: E402
from token_dashboard.db import init_db  # noqa: E402
from token_dashboard.pricing import set_plan  # noqa: E402
from token_dashboard.scanner import scan_sources  # noqa: E402
from token_dashboard.server import build_handler  # noqa: E402
from token_dashboard.tips import dismiss_tip  # noqa: E402

db = os.path.join(tempfile.mkdtemp(), "c.db")
init_db(db)
srcs = fg.fixture_sources()
scan_sources(srcs, db)
set_plan(db, "pro", "codex")
dismiss_tip(db, "claude:repeat-bash:make test")
port = int(sys.argv[1]) if len(sys.argv) > 1 else 8199
print("python fixture server on", port, flush=True)
http.server.ThreadingHTTPServer(("127.0.0.1", port), build_handler(db, str(fg.FIX / "claude"), srcs)).serve_forever()
