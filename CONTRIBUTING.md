# Contributing

- Code: Go under `go/`, the UI is plain JS in `web/` (no build step), embedded into the binary.
- Test: `go vet ./... && go test ./...`. `scripts/smoke.sh <binary>` runs a clean-home end-to-end check.
- `contract/golden/` holds the answers the original Python dashboard gave for the fixtures in `contract/fixtures/`. The Go server must keep matching them. Do not edit those files by hand, and do not change line endings in fixtures (Codex message ids embed byte offsets).
- Docs: `docs/` built with MkDocs (`pip install -r docs/requirements.txt && mkdocs build --strict`).
