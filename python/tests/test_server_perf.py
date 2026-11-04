import json
import queue
import sqlite3
import time
import unittest

from tests.test_server import ServerTests as _Base
from token_dashboard import server


class PerfTests(_Base):
    """Reuses the base fixture (one session in a temp DB)."""

    def setUp(self):
        server.bump_generation()
        super().setUp()

    def test_cached_until_generation_bumps(self):
        a = json.loads(self._get("/api/overview"))
        with sqlite3.connect(self.db) as c:   # data changes behind the cache's back
            c.execute("INSERT INTO messages (uuid, parent_uuid, session_id, project_slug, type, timestamp) "
                      "VALUES ('x',NULL,'s2','p','user','2026-04-20T00:00:00Z')")
            c.commit()
        self.assertEqual(json.loads(self._get("/api/overview")), a)          # served from cache
        server.bump_generation()                                              # what a scan does
        self.assertEqual(json.loads(self._get("/api/overview"))["sessions"], a["sessions"] + 1)

    def test_since_is_quantised_to_the_minute(self):
        self._get("/api/overview?since=2026-04-19T00:00:10.111Z")
        n = len(server._CACHE)
        self._get("/api/overview?since=2026-04-19T00:00:50.999Z")
        self.assertEqual(len(server._CACHE), n)

    def test_plan_post_invalidates(self):
        self._get("/api/overview")
        self.assertTrue(server._CACHE)
        import urllib.request
        urllib.request.urlopen(urllib.request.Request(
            f"http://127.0.0.1:{self.port}/api/plan", data=b'{"plan":"max"}', method="POST",
            headers={"Content-Type": "application/json"})).read()
        self.assertFalse(server._CACHE)

    def test_publish_reaches_every_subscriber(self):
        qs = [queue.Queue(), queue.Queue()]
        with server._SUBS_LOCK:
            server._SUBS.update(qs)
        try:
            server.publish({"type": "scan"})
            for q in qs:
                self.assertEqual(q.get(timeout=1)["type"], "scan")
        finally:
            with server._SUBS_LOCK:
                server._SUBS.difference_update(qs)

    def test_health_endpoints(self):
        self.assertTrue(json.loads(self._get("/healthz"))["ok"])
        self.assertTrue(json.loads(self._get("/readyz"))["ready"])

    def _raw(self, method, path, headers=None, body=None):
        import http.client
        c = http.client.HTTPConnection("127.0.0.1", self.port)
        c.request(method, path, body=body, headers=headers or {})
        r = c.getresponse(); r.read(); c.close()
        return r.status

    def test_foreign_host_rejected_but_probes_allowed(self):
        self.assertEqual(self._raw("GET", "/api/overview", {"Host": "evil.example"}), 403)
        self.assertEqual(self._raw("GET", "/", {"Host": "evil.example"}), 403)
        self.assertEqual(self._raw("GET", "/healthz", {"Host": "10.1.2.3:8191"}), 200)   # kubelet probes by pod IP
        self.assertEqual(self._raw("GET", "/api/overview", {"Host": "localhost:8191"}), 200)

    def test_allowed_hosts_env(self):
        import os
        os.environ["TOKEN_DASHBOARD_ALLOWED_HOSTS"] = "td.example.com"
        try:
            self.assertEqual(self._raw("GET", "/api/overview", {"Host": "td.example.com"}), 200)
        finally:
            del os.environ["TOKEN_DASHBOARD_ALLOWED_HOSTS"]

    def test_post_requires_json_content_type(self):
        self.assertEqual(self._raw("POST", "/api/plan", {"Content-Type": "text/plain"}, b'{"plan":"max"}'), 415)
        self.assertEqual(self._raw("POST", "/api/plan", {"Content-Type": "application/x-www-form-urlencoded"}, b"plan=max"), 415)
        self.assertEqual(self._raw("POST", "/api/plan", {"Content-Type": "application/json"}, b'{"plan":"api"}'), 200)

    def test_db_is_wal(self):
        with sqlite3.connect(self.db) as c:
            self.assertEqual(c.execute("PRAGMA journal_mode").fetchone()[0], "wal")


del _Base   # keep unittest from re-running the base tests under this module
if __name__ == "__main__":
    unittest.main()
