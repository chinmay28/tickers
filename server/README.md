# The Tickers server

One Go module, one command, one static binary. It serves the REST API, the web
client and the scheduled refresh loop from a single process with no runtime
dependencies.

```
server/
├── cmd/tickers/          entrypoint & CLI
└── internal/
    ├── version/          vMAJOR.MINOR.<commit count>
    ├── store/            SQLite: schema, migrations, every query
    ├── quotes/           quote providers (Yahoo Finance)
    ├── publish/          downstream publishing + the legacy payload
    ├── engine/           the refresh + publish cycle and its scheduler
    ├── api/              HTTP handlers
    └── web/              the web client, embedded with go:embed
```

Dependencies point one way: `api` → `engine` → {`quotes`, `publish`} →
`store`. See [docs/DESIGN.md](../docs/DESIGN.md) for why.

## Build

```bash
../scripts/build.sh                            # → server/bin/tickers
GOOS=linux GOARCH=arm64 ../scripts/build.sh    # → a Raspberry Pi binary
```

`build.sh` sets `CGO_ENABLED=0` and stamps the version. A bare `go build
./cmd/tickers` also works but reports version `v1.0.0` — patch `0` means an
unstamped development build.

The only third-party dependency is `modernc.org/sqlite`, a pure-Go SQLite. That
is what makes `CGO_ENABLED=0` and cross-compilation work.

## CLI

```
tickers serve [flags]     run the API, the web client and the refresh loop
tickers publish [flags]   run one refresh + publish cycle, then exit
tickers version           print the version
tickers help
```

| Flag | Env fallback | Default |
|---|---|---|
| `--db` | `TICKERS_DB` | `./data/tickers.sqlite` |
| `--port` | `PORT` | `8797` |
| `--host` | `HOST` | `0.0.0.0` |
| `--web-dist` | `WEB_DIST` | — (use the embedded client) |
| `--verbose` | `TICKERS_VERBOSE` | off |
| `--quote-base-url` | `TICKERS_QUOTE_BASE_URL` | Yahoo's |
| `--quote-timeout` | `TICKERS_QUOTE_TIMEOUT` | `20` (seconds) |
| `--quote-user-agent` | `TICKERS_QUOTE_USER_AGENT` | a browser string |

Flag > env > default. Both subcommands take the same flags.

The three `--quote-*` flags are a *fallback* the Settings page can override:
the real ordering is **stored setting > flag > env > built-in default**. They
exist so a systemd unit can be templated; the GUI is where they normally get
changed, and a change there is in force on the next request. See
[docs/DESIGN.md](../docs/DESIGN.md#configuration-precedence).

`tickers publish` is the original cron script's job exactly: fetch, publish,
exit. It prints per-destination results and exits non-zero if every symbol
failed, so a systemd timer or cron wrapper notices.

`serve` handles `SIGINT`/`SIGTERM` by stopping the refresh loop and draining
in-flight requests within 10 seconds. That matters for the upgrade path:
`scripts/quickstart.sh` sends `SIGTERM` before snapshotting the database, and
an unclean stop would mean snapshotting a half-written WAL.

## Working on the web client

The client lives at `internal/web/assets/` and is embedded at build time. To
edit it without recompiling on every change:

```bash
go run ./cmd/tickers serve --web-dist internal/web/assets
```

Then just reload the browser. Rebuild with `scripts/build.sh` when you're done
— that is what bakes the changes into the binary.

## Tests

```bash
go vet ./...
go test -race ./...
```

`-race` is not optional in CI: the refresh loop, the HTTP handlers and a manual
refresh all touch the engine concurrently.

What each suite is responsible for:

| Package | Pins |
|---|---|
| `store` | migrations apply once and never re-seed; symbol normalisation; origin promotion; pinned symbols sorting to the top and surviving a round trip; history excludes failures; sink validation |
| `quotes` | the chart-response parse, including the trailing-null series and the meta fallback; per-symbol failure isolation; the settings-precedence merge and reconfiguration while a fetch is in flight |
| `publish` | **the legacy payload, byte for byte**, and the PUT→POST fallback |
| `engine` | cycle counts, publish gating, snapshot ordering, cycle serialisation, pushing stored quote settings into the provider |
| `api` | status codes, the `/api/state` shape, replacing a symbol end to end, pinning from Settings ordering the watchlist and the payload, quote-source settings round-tripping and validation, static-asset and deep-link serving |

`internal/publish/publish_test.go` is the compatibility specification: if a
change would alter what an existing consumer receives, it fails there first.

## Version

`internal/version/version.go` holds `Major` and `Minor` as constants; `Patch` is
the repository's commit count, stamped at link time:

```
-ldflags "-X github.com/chinmay28/tickers/server/internal/version.Patch=$(git rev-list --count HEAD)"
```

`scripts/version.sh` is the one place that computes it, and reads `Major`/
`Minor` out of this file — keep the two constants in a form its `sed` can find.
A shallow clone reports patch `0` rather than an undercount.
