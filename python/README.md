# WATCH MY HARNESS: Python edition

The same dashboard as the Go build, in standard-library Python. No pip, no build step, no compiler.
Both editions read the same logs, share one web UI (`../web`) and one pricing table (`../pricing.json`), and have the same
screens: setup, Sources, Archive, tips, profile. Snapshots made by one edition import into the other.

```sh
./python/run.sh              # macOS / Linux
py python\cli.py dashboard   # Windows (or: python python\cli.py dashboard)
```

Needs Python 3.9 or newer. The dashboard opens at <http://localhost:8191>.

Other commands: `python3 python/cli.py scan | today | stats | tips | recommend`.
Flags match the Go edition: `--db`, `--projects-dir`, `--source NAME=KIND:PATH`, `--codex`, `--no-open`, `--no-scan`, `--app`.

Run the tests: `cd python && python3 -m unittest discover tests`.

Which edition? See the [install guide](../docs/install.md).
