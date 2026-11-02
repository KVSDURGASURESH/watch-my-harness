# Archive your history

Claude Code deletes its own session files after about 30 days. WATCH MY HARNESS keeps what it has already read in a local database (`~/.claude/token-dashboard.db`), so older history survives, but that database is then the only copy.

**Settings → Archive** protects it:

- **Snapshots** are compressed copies of the database. One is written automatically each month into `~/.watch-my-harness/archive/`. You can change the folder, for example to a synced one such as iCloud or Dropbox.
- **Restore / merge** brings a snapshot back on any machine. Rows you already have are kept, so repeating it is safe.
- **Export** downloads all messages as CSV or JSON.
- **Keep N months** (optional) removes older rows from the live database. It always takes a snapshot first.
