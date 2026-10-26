# Install

1. Download the archive for your system from the latest release and unpack it.
2. Run `watch-my-harness` (`watch-my-harness.exe` on Windows). It starts a local server and opens the dashboard in its own window using your installed Chrome, Edge or Chromium. If none is found it opens your default browser.
3. Complete the first-run screen: tick the harnesses to track and press **Start scan**.

Command line: `watch-my-harness scan`, `today`, `stats`, `tips`, `dashboard --no-open --port 8191`, `licence`, `version`.

## Unsigned builds

Beta builds are not code-signed. On macOS right-click → Open once (or `xattr -d com.apple.quarantine watch-my-harness`). On Windows choose **More info → Run anyway** in SmartScreen.

## Where data lives

A local SQLite cache at `~/.claude/token-dashboard.db` and the sources list at `~/.claude/token-dashboard-sources.json`. Your session logs are only read, never modified.
