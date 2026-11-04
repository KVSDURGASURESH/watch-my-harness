"""HTTP server: static frontend + JSON endpoints + SSE diff stream."""
from __future__ import annotations

import http.server
import json
import mimetypes
import os
import queue
import sqlite3
import urllib.request
import threading
import time
from pathlib import Path
from urllib.parse import urlparse, parse_qs

from .db import (
    overview_totals, expensive_prompts, project_summary,
    tool_token_breakdown, recent_sessions, session_turns,
    daily_token_breakdown, model_breakdown, skill_breakdown, source_breakdown,
)
from .pricing import load_pricing, cost_for, get_plan, set_plan, agent_plans
from .tips import all_tips, dismiss_tip
from .scanner import scan_sources, Progress
from . import archive as arc
from . import sources as srcmod
from . import __version__
from .skills import cached_catalog, tokens_for
from .profile import build_profile
from .recommendations import build_recommendations_markdown


def _shared(name: str) -> Path:
    """web/ and pricing.json are shared with the Go edition at the repo root; a flat
    checkout (the legacy layout) keeps them next to cli.py."""
    here = Path(__file__).resolve().parent.parent
    for base in (here, here.parent):
        if (base / name).exists():
            return base / name
    return here / name


WEB_ROOT = _shared("web")
PRICING_JSON = _shared("pricing.json")

# SSE fan-out: every connected client gets its own queue, so one event reaches every tab
# (a single shared queue hands each event to just one reader).
_SUBS: "set[queue.Queue]" = set()
_SUBS_LOCK = threading.Lock()


def publish(evt: dict) -> None:
    with _SUBS_LOCK:
        subs = list(_SUBS)
    for q in subs:
        try:
            q.put_nowait(evt)
        except queue.Full:
            pass  # a stalled client must not block the scanner


# Response cache. Aggregates only change when a scan ingests rows (or a plan/dismiss write),
# so each JSON body is cached per (path, query) and dropped when GEN advances.
_CACHE: dict = {}
_CACHE_LOCK = threading.Lock()
_CACHE_MAX = 400
GEN = 0
_NO_CACHE = ("/api/stream", "/api/scan", "/api/plan", "/api/sources", "/api/scan-status", "/api/config",
             "/api/setup/complete", "/api/archive", "/api/archive/export")


def bump_generation() -> None:
    global GEN
    with _CACHE_LOCK:
        GEN += 1
        _CACHE.clear()


def _cache_key(path: str, qs: dict):
    if not path.startswith("/api/") or path in _NO_CACHE:
        return None
    parts = []
    for k in sorted(qs):
        v = qs[k][0] if qs[k] else ""
        if k in ("since", "until"):
            v = v[:16]  # rolling windows send "now - N" ISO stamps; reuse within the same minute
        parts.append((k, v))
    return (path, tuple(parts))


def _cache_get(key):
    with _CACHE_LOCK:
        return _CACHE.get(key)


def _cache_put(key, gen: int, body: bytes) -> None:
    with _CACHE_LOCK:
        if gen != GEN:
            return  # data changed while this response was being computed
        if len(_CACHE) >= _CACHE_MAX:
            _CACHE.pop(next(iter(_CACHE)))
        _CACHE[key] = body

# Liveness/readiness state. The scan loop stamps HEARTBEAT every pass (even a
# failed one); /healthz goes 503 when it stops advancing, i.e. the scanner hung.
HEARTBEAT = {"ts": time.time(), "scan": False, "interval": 30.0, "last_error": None}
STALE_FACTOR = 6  # unhealthy after this many missed scan intervals (+ a floor)


def _scan_age() -> float:
    return time.time() - HEARTBEAT["ts"]


def _scan_stale() -> bool:
    return HEARTBEAT["scan"] and _scan_age() > max(120.0, HEARTBEAT["interval"] * STALE_FACTOR)


# Defence against DNS-rebinding and cross-site POSTs to a local service: only answer requests whose
# Host is a loopback name (plus TOKEN_DASHBOARD_ALLOWED_HOSTS, comma-separated, e.g. an ingress host).
# Health probes are exempt: the kubelet addresses the pod by IP.
_LOOPBACK = {"localhost", "127.0.0.1", "[::1]", "::1"}
_PROBE_PATHS = ("/healthz", "/readyz")
_ARCHIVE_POST = ("/api/archive/settings", "/api/archive/snapshot", "/api/archive/import", "/api/archive/prune")


def _host_allowed(host_header: str) -> bool:
    host = (host_header or "").strip().lower()
    if host.startswith("["):
        host = host.split("]")[0] + "]"
    else:
        host = host.rsplit(":", 1)[0] if host.count(":") == 1 else host
    extra = {h.strip().lower() for h in os.environ.get("TOKEN_DASHBOARD_ALLOWED_HOSTS", "").split(",") if h.strip()}
    return host in _LOOPBACK or host in extra


MAX_POST_BYTES = 1_000_000  # 1 MB — we only accept tiny JSON bodies (plan, tip key)
MAX_LIMIT = 1000


def _send_json(handler, obj, status: int = 200) -> None:
    body = json.dumps(obj, default=str).encode("utf-8")
    ck = getattr(handler, "_ckey", None)
    if ck is not None and status == 200:
        _cache_put(ck, handler._cgen, body)
    _write_json(handler, body, status)


def _write_json(handler, body: bytes, status: int = 200) -> None:
    handler.send_response(status)
    handler.send_header("Content-Type", "application/json")
    handler.send_header("Content-Length", str(len(body)))
    handler.send_header("Cache-Control", "no-store")
    handler.end_headers()
    handler.wfile.write(body)


def _send_error(handler, status: int, msg: str) -> None:
    _send_json(handler, {"error": msg}, status=status)


def _clamp_limit(raw, default: int) -> int:
    try:
        v = int(raw)
    except (TypeError, ValueError):
        return default
    return max(1, min(v, MAX_LIMIT))


def _serve_static(handler, rel: str) -> None:
    rel = rel.lstrip("/")
    p = (WEB_ROOT / rel).resolve()
    if not str(p).startswith(str(WEB_ROOT.resolve())) or not p.is_file():
        handler.send_response(404)
        handler.end_headers()
        return
    body = p.read_bytes()
    ctype, _ = mimetypes.guess_type(str(p))
    handler.send_response(200)
    handler.send_header("Content-Type", ctype or "application/octet-stream")
    handler.send_header("Content-Length", str(len(body)))
    handler.end_headers()
    handler.wfile.write(body)


class Runtime:
    """What the scanner, the setup screen and the Settings editors share: the live source list
    (re-resolved each scan, so UI edits apply without a restart), scan progress and the setup gate."""

    def __init__(self, db_path, projects_dir, sources=None, manager=None, gate=False):
        self.db_path = db_path
        self.manager = manager
        self._static = sources or [{"name": "claude", "kind": "claude", "path": projects_dir}]
        self.progress = Progress()
        self.gate = gate
        self.scan_lock = threading.Lock()

    def sources(self):
        return self.manager.effective() if self.manager else self._static

    def setup_complete(self) -> bool:
        return arc.get_kv(self.db_path, "setup_complete") is not None

    def has_messages(self) -> bool:
        with sqlite3.connect(self.db_path, timeout=5) as c:
            return c.execute("SELECT 1 FROM messages LIMIT 1").fetchone() is not None

    def scan_now(self) -> dict:
        with self.scan_lock:
            n = scan_sources(self.sources(), self.db_path, self.progress)
        if n["messages"] > 0 or n.get("tools", 0) > 0:
            bump_generation()
            _warm(self.db_path)
            publish({"type": "scan", "n": n, "ts": time.time()})
        return n

    def start_scan(self) -> bool:
        """Kick a background scan; False when one is already running."""
        if self.scan_lock.locked():
            return False

        def go():
            try:
                self.scan_now()
                arc.auto_if_due(self.db_path)
            except Exception as e:
                HEARTBEAT["last_error"] = str(e)
                publish({"type": "error", "message": str(e)})
            publish({"type": "scan-done", "ts": time.time()})
        threading.Thread(target=go, daemon=True).start()
        return True


def srcmod_licence() -> str:
    """Placeholder licence field from ~/.claude/token-dashboard-config.json: stored and shown, never enforced."""
    p = Path(os.environ.get("TOKEN_DASHBOARD_CONFIG") or Path.home() / ".claude" / "token-dashboard-config.json")
    try:
        return json.loads(p.read_text(encoding="utf-8")).get("licence") or "free"
    except (OSError, ValueError, AttributeError):
        return "free"


def _effective_json(srcs) -> list:
    return [{"name": x["name"], "kind": x["kind"], "path": x["path"],
             **({"model_alias": x["options"]["model_alias"]} if x.get("options", {}).get("model_alias") else {})}
            for x in srcs]


def build_handler(db_path: str, projects_dir: str, sources=None, runtime=None):
    rt = runtime or Runtime(db_path, projects_dir, sources)
    pricing = load_pricing(PRICING_JSON)

    class H(http.server.BaseHTTPRequestHandler):
        def log_message(self, fmt, *args):
            pass

        def do_HEAD(self):
            return self.do_GET()

        def do_GET(self):
            self._ckey = None
            url = urlparse(self.path)
            if url.path not in _PROBE_PATHS and not _host_allowed(self.headers.get("Host", "")):
                return _send_error(self, 403, "host not allowed (set TOKEN_DASHBOARD_ALLOWED_HOSTS)")
            qs = parse_qs(url.query or "")
            path = url.path
            since = qs.get("since", [None])[0]
            until = qs.get("until", [None])[0]
            source = qs.get("source", [None])[0] or None
            ck = _cache_key(path, qs)
            if ck is not None:
                hit = _cache_get(ck)
                if hit is not None:
                    return _write_json(self, hit)
                self._ckey, self._cgen = ck, GEN
            if path == "/healthz":  # liveness: process serves HTTP and the scanner is advancing
                stale = _scan_stale()
                return _send_json(self, {"ok": not stale, "scan_age_s": round(_scan_age(), 1),
                                         "last_error": HEARTBEAT["last_error"]}, 503 if stale else 200)
            if path == "/readyz":  # readiness: the database is queryable
                try:
                    with sqlite3.connect(db_path, timeout=3) as c:
                        c.execute("SELECT 1 FROM messages LIMIT 1").fetchall()
                except sqlite3.Error as e:
                    return _send_json(self, {"ready": False, "error": str(e)}, 503)
                return _send_json(self, {"ready": True})
            if path in ("/", "/index.html"):
                return _serve_static(self, "index.html")
            if path.startswith("/web/"):
                return _serve_static(self, path[5:])
            if path == "/api/overview":
                totals = overview_totals(db_path, since, until, source)
                cost_usd, priced, models = 0.0, False, 0
                for m in model_breakdown(db_path, since, until, source):
                    models += 1
                    c = cost_for(m["model"], m, pricing)
                    if c["usd"] is not None:
                        cost_usd += c["usd"]
                        priced = True
                # None, not 0: an agent with no priced models isn't free, it's unknown.
                totals["cost_usd"] = round(cost_usd, 4) if priced or not models else None
                return _send_json(self, totals)
            if path == "/api/prompts":
                limit = _clamp_limit(qs.get("limit", ["50"])[0], 50)
                sort = qs.get("sort", ["tokens"])[0]
                rows = expensive_prompts(db_path, limit=limit, sort=sort, source=source)
                for r in rows:
                    c = cost_for(r["model"], {
                        "input_tokens": 0, "output_tokens": 0,
                        "cache_read_tokens": r["cache_read_tokens"],
                        "cache_create_5m_tokens": 0, "cache_create_1h_tokens": 0,
                    }, pricing)
                    r["estimated_cost_usd"] = c["usd"]
                return _send_json(self, rows)
            if path == "/api/projects":
                return _send_json(self, project_summary(db_path, since, until, source))
            if path == "/api/profile":
                return _send_json(self, build_profile(db_path, since, until, source))
            if path == "/api/tools":
                return _send_json(self, tool_token_breakdown(db_path, since, until, source))
            if path == "/api/sessions":
                return _send_json(self, recent_sessions(
                    db_path, limit=_clamp_limit(qs.get("limit", ["20"])[0], 20),
                    since=since, until=until, source=source,
                ))
            if path == "/api/daily":
                return _send_json(self, daily_token_breakdown(db_path, since, until, source))
            if path == "/api/skills":
                rows = skill_breakdown(db_path, since, until, source)
                catalog = cached_catalog()
                for r in rows:
                    r["tokens_per_call"] = tokens_for(r["skill"], catalog)
                return _send_json(self, rows)
            if path == "/api/by-source":
                rows = source_breakdown(db_path, since, until)
                for r in rows:
                    costs = [cost_for(m["model"], m, pricing)["usd"]
                             for m in model_breakdown(db_path, since, until, r["source"])]
                    priced = [c for c in costs if c is not None]
                    # None = no model of this agent is in pricing.json; a $0 would read as "free".
                    r["cost_usd"] = round(sum(priced), 4) if priced else None
                    r["unpriced_models"] = len(costs) - len(priced)
                    plan = get_plan(db_path, source=r["source"])
                    p = pricing["plans"].get(plan, pricing["plans"]["api"])
                    r["plan"], r["plan_label"], r["plan_monthly"] = plan, p["label"], p["monthly"]
                return _send_json(self, rows)
            if path == "/api/by-model":
                rows = model_breakdown(db_path, since, until, source)
                for r in rows:
                    c = cost_for(r["model"], r, pricing)
                    r["cost_usd"] = c["usd"]
                    r["cost_estimated"] = c["estimated"]
                return _send_json(self, rows)
            if path.startswith("/api/sessions/"):
                sid = path.rsplit("/", 1)[1]
                return _send_json(self, session_turns(db_path, sid))
            if path == "/api/tips":
                return _send_json(self, all_tips(db_path, source=source))
            if path == "/api/recommendations":
                return _send_json(self, {"markdown": build_recommendations_markdown(db_path, source=source)})
            if path == "/api/plan":
                with sqlite3.connect(db_path, timeout=5) as c:
                    configured = c.execute("SELECT 1 FROM plan WHERE k='plan'").fetchone() is not None
                return _send_json(self, {"plan": get_plan(db_path), "configured": configured,
                                         "agent_plans": agent_plans(db_path), "pricing": pricing})
            if path == "/api/scan":
                return _send_json(self, rt.scan_now())
            if path == "/api/scan-status":
                return _send_json(self, rt.progress.snapshot())
            if path == "/api/config":
                return _send_json(self, {"licence": srcmod_licence(), "version": __version__})
            if path == "/api/sources":
                m = rt.manager
                if m is None:
                    return _send_json(self, {"sources": [], "effective": _effective_json(rt.sources()),
                                             "detected": srcmod.detect(projects_dir), "setup_complete": True,
                                             "config_path": "", "pinned": True,
                                             "kinds": sorted(srcmod.KINDS)})
                try:
                    entries, eff = m.entries(), m.effective()
                except (ValueError, OSError) as e:
                    return _send_error(self, 500, str(e))
                return _send_json(self, {"sources": entries, "effective": _effective_json(eff),
                                         "detected": srcmod.detect(m.claude_dir),
                                         "setup_complete": rt.setup_complete(),
                                         "config_path": str(m.config_path), "pinned": m.pinned,
                                         "kinds": ["claude", "codex", "hermes", "generic"]})
            if path == "/api/archive":
                st = arc.load(db_path)
                return _send_json(self, {"settings": st, "snapshots": arc.list_snapshots(st["dir"]),
                                         "default_dir": arc.default_dir()})
            if path == "/api/archive/export":
                fmt = "json" if (qs.get("format") or [""])[0] == "json" else "csv"
                self.send_response(200)
                self.send_header("Content-Type", "application/json" if fmt == "json" else "text/csv")
                self.send_header("Content-Disposition", f'attachment; filename="watch-my-harness-export.{fmt}"')
                self.send_header("Cache-Control", "no-store")
                self.end_headers()
                if self.command != "HEAD":
                    for chunk in arc.export_chunks(db_path, fmt):
                        self.wfile.write(chunk)
                return
            if path == "/api/stream":
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-store")
                self.send_header("Connection", "keep-alive")
                self.end_headers()
                q: "queue.Queue[dict]" = queue.Queue(maxsize=100)
                with _SUBS_LOCK:
                    _SUBS.add(q)
                try:
                    while True:
                        try:
                            evt = q.get(timeout=15)
                            chunk = f"data: {json.dumps(evt, default=str)}\n\n".encode()
                        except queue.Empty:
                            chunk = b": ping\n\n"
                        try:
                            self.wfile.write(chunk)
                            self.wfile.flush()
                        except (BrokenPipeError, ConnectionResetError):
                            return
                finally:
                    with _SUBS_LOCK:
                        _SUBS.discard(q)
            self.send_response(404)
            self.end_headers()

        def do_POST(self):
            self._ckey = None
            url = urlparse(self.path)
            if not _host_allowed(self.headers.get("Host", "")):
                return _send_error(self, 403, "host not allowed (set TOKEN_DASHBOARD_ALLOWED_HOSTS)")
            # JSON-only: a cross-site <form> can only send urlencoded/multipart/text, and a fetch with
            # application/json triggers a CORS preflight that this server never approves.
            if not (self.headers.get("Content-Type", "").split(";")[0].strip().lower() == "application/json"):
                return _send_error(self, 415, "Content-Type must be application/json")
            try:
                length = int(self.headers.get("Content-Length") or 0)
            except ValueError:
                return _send_error(self, 400, "invalid Content-Length")
            if length < 0 or length > MAX_POST_BYTES:
                return _send_error(self, 413, f"body too large (max {MAX_POST_BYTES} bytes)")
            try:
                body = json.loads(self.rfile.read(length) or b"{}") if length else {}
            except json.JSONDecodeError:
                return _send_error(self, 400, "invalid JSON")
            if not isinstance(body, dict):
                return _send_error(self, 400, "body must be a JSON object")
            if url.path == "/api/plan":
                set_plan(db_path, body.get("plan", "api"), body.get("source") or None)
                bump_generation()
                return _send_json(self, {"ok": True})
            if url.path == "/api/tips/dismiss":
                dismiss_tip(db_path, body.get("key", ""))
                bump_generation()
                threading.Thread(target=_warm, args=(db_path,), daemon=True).start()
                return _send_json(self, {"ok": True})
            if url.path in _ARCHIVE_POST:
                return self._archive_post(url.path, body)
            if url.path == "/api/sources":
                return self._sources_post(body)
            if url.path == "/api/setup/complete":
                return self._setup_post(body)
            if url.path == "/api/scan":
                return _send_json(self, {"ok": True, "started": rt.start_scan()})
            self.send_response(404)
            self.end_headers()

        def _parse_entries(self, body):
            es = body.get("sources")
            srcmod.validate(es)
            return es

        def _sources_post(self, body):
            if rt.manager is None or rt.manager.pinned:
                return _send_error(self, 409, "sources are pinned by --projects-dir / CLAUDE_PROJECTS_DIR")
            try:
                rt.manager.set_entries(self._parse_entries(body))
            except ValueError as e:
                return _send_error(self, 400, str(e))
            except OSError as e:
                return _send_error(self, 500, str(e))
            bump_generation()
            rt.start_scan()
            return _send_json(self, {"ok": True})

        def _setup_post(self, body):
            if "sources" in body and rt.manager is not None and not rt.manager.pinned:
                try:
                    rt.manager.set_entries(self._parse_entries(body))
                except ValueError as e:
                    return _send_error(self, 400, str(e))
                except OSError as e:
                    return _send_error(self, 500, str(e))
            plan = body.get("plan")
            if isinstance(plan, str) and plan:
                set_plan(db_path, plan, None)
            arc.set_kv(db_path, "setup_complete", "1")
            bump_generation()
            rt.start_scan()
            return _send_json(self, {"ok": True})

        def _archive_post(self, path, body):
            st = arc.load(db_path)
            try:
                if path == "/api/archive/settings":
                    if isinstance(body.get("dir"), str):
                        st["dir"] = body["dir"]
                    if isinstance(body.get("auto"), bool):
                        st["auto"] = body["auto"]
                    if isinstance(body.get("keep_months"), (int, float)) and not isinstance(body["keep_months"], bool):
                        st["keep_months"] = int(body["keep_months"])
                    arc.save(db_path, st)
                    return _send_json(self, {"ok": True})
                if path == "/api/archive/snapshot":
                    return _send_json(self, {"ok": True, "name": arc.take(db_path, st["dir"])})
                if path == "/api/archive/import":
                    n = arc.import_snapshot(db_path, st["dir"], str(body.get("name") or ""))
                    bump_generation()
                    return _send_json(self, {"ok": True, "added": n})
                if st["keep_months"] <= 0:  # /api/archive/prune
                    return _send_error(self, 400, "set 'keep months' above 0 first")
                try:
                    name = arc.take(db_path, st["dir"])  # always snapshot before deleting
                except Exception as e:
                    return _send_error(self, 500, f"snapshot failed, nothing deleted: {e}")
                n = arc.prune(db_path, st["keep_months"])
                bump_generation()
                return _send_json(self, {"ok": True, "removed": n, "snapshot": name})
            except arc.ArchiveError as e:
                return _send_error(self, 400, str(e))
            except (OSError, sqlite3.Error) as e:
                return _send_error(self, 500, str(e))

    return H


def _warm(db_path: str) -> None:
    """Precompute the slowest endpoint (tips: ~100 queries/agent) off the request path, so the
    next /api/tips is a cache hit. Computed once per data generation instead of once per page load."""
    gen = GEN
    try:
        body = json.dumps(all_tips(db_path), default=str).encode("utf-8")
        _cache_put(("/api/tips", ()), gen, body)
    except Exception:
        pass


def _scan_loop(rt: "Runtime", interval: float = 30.0):
    HEARTBEAT.update(scan=True, interval=interval)
    while True:
        try:
            if rt.gate and not rt.setup_complete() and not rt.has_messages():
                pass  # first run: wait for the setup screen
            else:
                rt.scan_now()
                arc.auto_if_due(rt.db_path)
            HEARTBEAT["last_error"] = None
        except Exception as e:
            HEARTBEAT["last_error"] = str(e)
            publish({"type": "error", "message": str(e)})
        HEARTBEAT["ts"] = time.time()
        time.sleep(interval)


def _watchdog(port: int, every: float = 30.0, strikes: int = 3):
    """Exit the process when it goes stale (HTTP unresponsive or scanner hung) so a
    restart policy (docker `restart: unless-stopped`) brings it back. Docker itself
    only marks a container unhealthy; it never restarts one."""
    bad = 0
    while True:
        time.sleep(every)
        try:
            urllib.request.urlopen(f"http://127.0.0.1:{port}/healthz", timeout=5).read()
            bad = 0
        except Exception:
            bad += 1
            if bad >= strikes:
                print("token-dashboard watchdog: unhealthy, exiting for restart", flush=True)
                os._exit(1)


def run(host: str, port: int, db_path: str, projects_dir: str, scan: bool = True, sources=None,
        manager=None, require_setup: bool = False):
    rt = Runtime(db_path, projects_dir, sources, manager, gate=require_setup)
    threading.Thread(target=_warm, args=(db_path,), daemon=True).start()
    if scan:
        threading.Thread(target=_scan_loop, args=(rt,), daemon=True).start()
    H = build_handler(db_path, projects_dir, runtime=rt)
    if os.environ.get("TOKEN_DASHBOARD_WATCHDOG") == "1":
        threading.Thread(target=_watchdog, args=(port,), daemon=True).start()
    httpd = http.server.ThreadingHTTPServer((host, port), H)
    httpd.serve_forever()
