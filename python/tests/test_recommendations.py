import os
import tempfile
import unittest

from token_dashboard.db import init_db, connect
from token_dashboard.recommendations import build_recommendations_markdown, HEADER


class RecommendationsTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp()
        self.db = os.path.join(self.tmp, "t.db")
        init_db(self.db)

    def test_no_tips_returns_empty_string(self):
        md = build_recommendations_markdown(self.db, today_iso="2026-04-19T00:00:00")
        self.assertEqual(md, "")

    def test_active_tip_becomes_a_draft_bullet(self):
        with connect(self.db) as c:
            c.execute("""INSERT INTO messages (uuid, session_id, project_slug, type, timestamp,
                model, input_tokens, output_tokens, cache_read_tokens,
                cache_create_5m_tokens, cache_create_1h_tokens) VALUES
                ('m1', 's', 'projX', 'assistant', '2026-04-15T00:00:00Z', 'claude-opus-4-7',
                 100, 100, 10, 1000000, 0)""")
            c.commit()
        md = build_recommendations_markdown(self.db, today_iso="2026-04-19T00:00:00")
        self.assertIn(HEADER, md)
        self.assertIn("Low cache hit rate in projX", md)
        self.assertIn("nothing here is applied automatically", md)

    def test_dismissed_tip_is_excluded_from_draft(self):
        from token_dashboard.tips import cache_discipline_tips, dismiss_tip
        with connect(self.db) as c:
            c.execute("""INSERT INTO messages (uuid, session_id, project_slug, type, timestamp,
                model, input_tokens, output_tokens, cache_read_tokens,
                cache_create_5m_tokens, cache_create_1h_tokens) VALUES
                ('m1', 's', 'projY', 'assistant', '2026-04-15T00:00:00Z', 'claude-opus-4-7',
                 100, 100, 10, 1000000, 0)""")
            c.commit()
        tips_before = cache_discipline_tips(self.db, today_iso="2026-04-19T00:00:00")
        dismiss_tip(self.db, tips_before[0]["key"])
        md = build_recommendations_markdown(self.db, today_iso="2026-04-19T00:00:00")
        self.assertEqual(md, "")


if __name__ == "__main__":
    unittest.main()
