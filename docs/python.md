# Python edition

WATCH MY HARNESS ships in two editions. They read the same logs, show the same screens, share one web UI and one pricing table, and
can read each other's archive snapshots. Pick whichever suits you.

| | Go (prebuilt file or `go install`) | Python (clone and run) |
|---|---|---|
| You need | nothing (prebuilt) or Go | Python 3.9+ (already on most Macs and Linux machines) |
| Start | double-click / `watch-my-harness` | `./python/run.sh` |
| Signing warnings | yes, the beta binaries are unsigned | none, you run readable source |
| Install steps | download and unzip | `git clone` |
| Speed on a big history | faster first scan | fine, a bit slower on years of logs |
| Updating | download again | `git pull` |

## Run it

```sh
git clone https://github.com/KVSDURGASURESH/watch-my-harness
cd watch-my-harness
./python/run.sh
```

On Windows: `py python\cli.py dashboard`.

The dashboard opens at <http://localhost:8191>. First run shows the same setup screen as the Go edition; change sources later in
**Settings → Sources**.

## Same data, either edition

Both use the database at `~/.claude/token-dashboard.db` and the sources file `~/.claude/token-dashboard-sources.json`, so you can switch
editions without losing anything. Do not run both at once: they would fight over the same SQLite file and port 8191.

Archive snapshots (`*.db.gz`) made by either edition import into the other.

## Command line

```sh
python3 python/cli.py scan        # refresh the database
python3 python/cli.py today       # today's totals
python3 python/cli.py stats       # all-time totals
python3 python/cli.py tips        # token-saving suggestions
python3 python/cli.py recommend   # draft CLAUDE.md block
python3 python/cli.py dashboard --source codex=codex:~/.codex/sessions
```
