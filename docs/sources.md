# Tracking your harnesses

Defaults: `claude` (`~/.claude/projects`), `codex` (`~/.codex/sessions`), `hermes` (`~/.hermes`). All are ticked on first run; untick, edit the path, or remove any of them. Add your own in **Settings → Sources** or with `--source name=kind:path`.

Kinds: `claude`, `codex`, `hermes`, `generic`.

## The `generic` format

Any tool can be tracked by writing one JSON object per API call, one per line:

```json
{"session_id":"s1","timestamp":"2026-10-25T10:00:00Z","model":"my-model","input_tokens":1200,"output_tokens":300,"cache_read_tokens":0,"project":"my-app","prompt":"optional text","tools":["Read","Edit"]}
```

Required: `session_id`, `timestamp`. `input_tokens` means uncached input. Optional: `cache_read_tokens`, `cwd`, `project`, `prompt`, `tools`.
