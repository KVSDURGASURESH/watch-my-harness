#!/usr/bin/env bash
# WATCH MY HARNESS, Python edition: start the dashboard from a checkout.
# No pip, no build step. Needs Python 3.9+. Usage: ./python/run.sh [dashboard flags], e.g. --no-open
set -e
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PY="$(command -v python3 || command -v python || true)"
if [ -z "$PY" ] || ! "$PY" -c 'import sys; sys.exit(sys.version_info < (3, 9))' 2>/dev/null; then
  echo "WATCH MY HARNESS needs Python 3.9 or newer (https://www.python.org/downloads/)." >&2
  exit 1
fi
echo "WATCH MY HARNESS (Python)  ->  http://localhost:${PORT:-8191}"
exec "$PY" "$HERE/cli.py" dashboard "$@"
