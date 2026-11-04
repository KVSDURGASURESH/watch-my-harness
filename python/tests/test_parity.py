"""Endpoints the Python edition shares with the Go one: archive, sources, setup, scan-status, config."""
import csv
import gzip
import http.server
import io
import json
import os
import socket
import sqlite3
import tempfile
import threading
import unittest
import urllib.error
import urllib.request
from pathlib import Path

from token_dashboard import archive
from token_dashboard.db import init_db
from token_dashboard.server import Runtime, build_handler
from token_dashboard.sources import Manager

MSG = ("INSERT INTO messages (uuid, parent_uuid, session_id, project_slug, type, timestamp, model, "
       "input_tokens, output_tokens, cache_read_tokens, cache_create_5m_tokens, cache_create_1h_tokens) "
       "VALUES (?,NULL,'s','p','assistant',?,'claude-haiku-4-5',1,2,0,0,0)")


def _port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p


def _seed(db, rows):
    init_db(db)
    with sqlite3.connect(db) as c:
        for u, ts in rows:
            c.execute(MSG, (u, ts))
        c.commit()


def _isolate_home(case, home):
    """Default archive folder, sources file and config live under $HOME; never touch the real one."""
    saved = {k: os.environ.get(k) for k in ("HOME", "USERPROFILE")}
    os.environ["HOME"] = os.environ["USERPROFILE"] = home

    def restore():
        for k, v in saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v
    case.addCleanup(restore)


class _Base(unittest.TestCase):
    pinned = False

    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        _isolate_home(self, self.tmp)
        self.db = os.path.join(self.tmp, "t.db")
        self.arc = os.path.join(self.tmp, "arc")
        _seed(self.db, [("old", "2020-01-01T00:00:00Z"), ("new", "2999-01-01T00:00:00Z")])
        proj = os.path.join(self.tmp, "projects")
        os.makedirs(proj)
        self.mgr = Manager(proj, pinned=self.pinned, config_path=Path(self.tmp) / "sources.json")
        self.rt = Runtime(self.db, proj, manager=self.mgr)
        self.port = _port()
        self.httpd = http.server.ThreadingHTTPServer(("127.0.0.1", self.port), build_handler(self.db, proj, runtime=self.rt))
        threading.Thread(target=self.httpd.serve_forever, daemon=True).start()

    def tearDown(self):
        with self.rt.scan_lock:  # let a background scan from start_scan() finish first
            pass
        self.httpd.shutdown()
        self.httpd.server_close()

    def req(self, path, body=None):
        r = urllib.request.Request(f"http://127.0.0.1:{self.port}{path}",
                                   data=None if body is None else json.dumps(body).encode(),
                                   headers={"Content-Type": "application/json"} if body is not None else {})
        try:
            with urllib.request.urlopen(r) as f:
                return f.status, f.read()
        except urllib.error.HTTPError as e:
            return e.code, e.read()

    def j(self, path, body=None):
        st, b = self.req(path, body)
        return st, json.loads(b)

    def count(self, db=None):
        with sqlite3.connect(db or self.db) as c:
            return c.execute("SELECT COUNT(*) FROM messages").fetchone()[0]


class ArchiveTests(_Base):
    def test_defaults_and_settings(self):
        st, b = self.j("/api/archive")
        self.assertEqual(st, 200)
        self.assertEqual(b["settings"]["keep_months"], 0)
        self.assertTrue(b["settings"]["auto"])
        self.assertEqual(b["snapshots"], [])
        self.assertEqual(self.j("/api/archive/settings", {"dir": self.arc, "auto": False, "keep_months": 6})[0], 200)
        b = self.j("/api/archive")[1]["settings"]
        self.assertEqual((b["dir"], b["auto"], b["keep_months"]), (self.arc, False, 6))

    def test_settings_validation(self):
        self.assertEqual(self.j("/api/archive/settings", {"dir": "relative/path"})[0], 400)
        self.assertEqual(self.j("/api/archive/settings", {"keep_months": -1})[0], 400)

    def test_snapshot_import_merge_existing_wins(self):
        self.j("/api/archive/settings", {"dir": self.arc})
        st, b = self.j("/api/archive/snapshot", {})
        self.assertEqual(st, 200)
        name = b["name"]
        self.assertTrue(name.endswith(".db.gz"))
        self.assertEqual([s["name"] for s in self.j("/api/archive")[1]["snapshots"]], [name])
        with sqlite3.connect(self.db) as c:  # live DB loses a row, then gets a different one
            c.execute("DELETE FROM messages WHERE uuid='old'")
            c.execute(MSG, ("extra", "2999-02-01T00:00:00Z"))
            c.commit()
        st, b = self.j("/api/archive/import", {"name": name})
        self.assertEqual((st, b["added"]), (200, 1))  # only "old" is new to the live DB
        self.assertEqual(self.count(), 3)
        self.assertEqual(self.j("/api/archive/import", {"name": "../evil.txt"})[0], 400)
        self.assertEqual(self.j("/api/archive/import", {"name": "missing.db.gz"})[0], 400)

    def test_prune_snapshots_first(self):
        self.assertEqual(self.j("/api/archive/prune", {})[0], 400)  # keep_months == 0
        self.j("/api/archive/settings", {"dir": self.arc, "keep_months": 12})
        st, b = self.j("/api/archive/prune", {})
        self.assertEqual((st, b["removed"]), (200, 1))
        self.assertEqual(self.count(), 1)
        snap = os.path.join(self.arc, b["snapshot"])
        with gzip.open(snap, "rb") as f, open(snap + ".raw", "wb") as o:
            o.write(f.read())
        self.assertEqual(self.count(snap + ".raw"), 2)  # snapshot kept the pruned row

    def test_export(self):
        st, body = self.req("/api/archive/export?format=csv")
        rows = list(csv.reader(io.StringIO(body.decode())))
        self.assertEqual(rows[0][:3], ["timestamp", "source", "session_id"])
        self.assertEqual(len(rows), 3)
        st, body = self.req("/api/archive/export?format=json")
        data = json.loads(body)
        self.assertEqual(len(data), 2)
        self.assertEqual(data[0]["input_tokens"], 1)

    def test_auto_once_per_month(self):
        self.j("/api/archive/settings", {"dir": self.arc, "auto": True})
        archive.auto_if_due(self.db)
        archive.auto_if_due(self.db)
        self.assertEqual(len(archive.list_snapshots(self.arc)), 1)


class SourcesTests(_Base):
    def test_get_and_edit(self):
        st, b = self.j("/api/sources")
        self.assertEqual((st, b["sources"], b["pinned"], b["setup_complete"]), (200, [], False, False))
        self.assertEqual([e["name"] for e in b["effective"]], ["claude"])
        self.assertEqual([d["name"] for d in b["detected"]], ["claude", "codex", "hermes"])
        rows = [{"name": "bot", "kind": "generic", "path": "/tmp/x.jsonl"},
                {"name": "off", "kind": "codex", "path": "/tmp/c", "enabled": False}]
        self.assertEqual(self.j("/api/sources", {"sources": rows})[0], 200)
        b = self.j("/api/sources")[1]
        self.assertEqual([e["name"] for e in b["sources"]], ["bot", "off"])
        self.assertEqual([e["name"] for e in b["effective"]], ["claude", "bot"])  # disabled one skipped

    def test_validation(self):
        for bad in ([{"name": "x y", "kind": "claude", "path": "/p"}], [{"name": "a", "kind": "nope", "path": "/p"}],
                    [{"name": "a", "kind": "claude", "path": " "}],
                    [{"name": "a", "kind": "claude", "path": "/p"}, {"name": "a", "kind": "codex", "path": "/q"}]):
            self.assertEqual(self.j("/api/sources", {"sources": bad})[0], 400, bad)

    def test_setup_complete(self):
        st, b = self.j("/api/setup/complete", {"sources": [{"name": "c", "kind": "codex", "path": "/nope"}], "plan": "max5x"})
        self.assertEqual((st, b), (200, {"ok": True}))
        info = self.j("/api/sources")[1]
        self.assertTrue(info["setup_complete"])
        self.assertEqual([e["name"] for e in info["sources"]], ["c"])
        self.assertEqual(self.j("/api/plan")[1]["plan"], "max5x")

    def test_scan_status_and_config(self):
        self.assertEqual(self.j("/api/scan", {})[1]["ok"], True)
        s = self.j("/api/scan-status")[1]
        for k in ("running", "files_total", "files_done", "messages", "tools", "current", "started_ms", "finished_ms", "error"):
            self.assertIn(k, s)
        c = self.j("/api/config")[1]
        self.assertEqual(c["licence"], "free")
        self.assertTrue(c["version"])


class PinnedTests(_Base):
    pinned = True

    def test_pinned_rejects_edits(self):
        self.assertTrue(self.j("/api/sources")[1]["pinned"])
        self.assertEqual(self.j("/api/sources", {"sources": []})[0], 409)


class ScanProgressTests(unittest.TestCase):
    def test_progress_counts_files(self):
        tmp = tempfile.mkdtemp()
        _isolate_home(self, tmp)
        db = os.path.join(tmp, "t.db")
        init_db(db)
        proj = os.path.join(tmp, "p", "slug")
        os.makedirs(proj)
        fx = Path(__file__).parent / "fixtures" / "sample_session.jsonl"
        Path(proj, "a.jsonl").write_bytes(fx.read_bytes())
        rt = Runtime(db, os.path.join(tmp, "p"))
        n = rt.scan_now()
        s = rt.progress.snapshot()
        self.assertEqual((s["running"], s["files_total"], s["files_done"]), (False, 1, 1))
        self.assertEqual(s["messages"], n["messages"])
        self.assertGreater(s["finished_ms"], 0)
        rt.scan_now()  # nothing changed: every file is skipped but still counted
        s = rt.progress.snapshot()
        self.assertEqual((s["files_total"], s["files_done"], s["messages"]), (1, 1, 0))


if __name__ == "__main__":
    unittest.main()
