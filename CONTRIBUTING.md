# Contributing

- Code: Go under `go/`, the UI is plain JS in `web/` (no build step), embedded into the binary.
- Test: `go vet ./... && go test ./...`. `scripts/smoke.sh <binary>` runs a clean-home end-to-end check.
- `contract/golden/` holds the answers the original Python dashboard gave for the fixtures in `contract/fixtures/`. Both the Go server and the Python edition (`python/`) must keep matching them: CI replays them against each (`go test ./go/internal/contract`, and the `python` workflow's `contract` job). Do not edit those files by hand, and do not change line endings in fixtures (Codex message ids embed byte offsets).
- Docs: `docs/` built with MkDocs (`pip install -r docs/requirements.txt && mkdocs build --strict`).

- The Python edition lives in `python/` and shares `web/` and `pricing.json` with Go. A change to an API endpoint or to the UI's expectations needs doing in both servers. Run its tests with `python3 -m unittest discover -s python/tests -t python`.
