# Build from source

Requirements: Go (the version in `go.mod`). No CGO, no Node. (The Python edition needs no build at all; see [Python edition](python.md).)

```sh
git clone https://github.com/KVSDURGASURESH/watch-my-harness
cd watch-my-harness
go build -o watch-my-harness ./go/cmd/watch-my-harness
./watch-my-harness
```

Or install straight from the module: `go install github.com/KVSDURGASURESH/watch-my-harness/go/cmd/watch-my-harness@latest`.

The web UI in `web/` is embedded in the binary, so one file is all you ship.

## Tests

```sh
go vet ./... && go test ./...
scripts/smoke.sh ./watch-my-harness     # fresh home, fixture logs, setup, scan, UI, archive
```

CI runs both on Linux, macOS and Windows.

## Cross-compile

```sh
GOOS=windows GOARCH=amd64 go build -o watch-my-harness.exe ./go/cmd/watch-my-harness
```

Releases are built with GoReleaser when a `v*` tag is pushed.
