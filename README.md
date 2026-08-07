<div align="center">
  <img src="server/internal/web/assets/icon.svg" alt="" width="96" height="96" />
  <h1>Tickers</h1>
  <p><em>A self-hosted watchlist that fetches quotes on a schedule and publishes them to your own endpoints.</em></p>
</div>

Tickers grew out of [a 50-line Python cron script](./legacy/update_minion_quotes.py)
that fetched seven hardcoded symbols with `yfinance` and PUT them into a
home-automation key-value store. It did one useful thing, and changing the
watchlist meant editing the source and redeploying.

This is that script as a proper application — built the same way as
[CountRoster](https://github.com/chinmay28/countroster):

- **A GUI for the watchlist.** Add, replace, relabel, pause, reorder and remove
  symbols from a browser. The seven symbols the script hardcoded ship as
  *placeholders* the app invites you to replace — the first thing you see is a
  **Replace** button on each.
- **Backwards-compatible publishing.** The payload is byte-for-byte the one the
  script wrote — flat map of symbol → 2-decimal string, `"N/A"` for a failure,
  a `"timestamp"` in `MM/DD HH:MM:SS` — with the same PUT-then-POST fallback.
  Anything already reading that entry keeps working, unchanged.
- **Many destinations, not one.** Publish the same snapshot to several
  key-value endpoints, each with its own key, category, format and timeout.
- **One static binary.** No Python, no Node, no runtime dependencies. The web
  client is embedded with `go:embed`, so `go build` is the whole build.
- **Non-disruptive upgrades.** Re-run one command: it snapshots the database,
  swaps code in, health-checks the result, and rolls back — data and all — if
  the new version is unhealthy.
- **No accounts, no auth.** Meant to run on a trusted network (your LAN, a
  Tailscale tailnet, a VPN). Anyone who can reach the server can use it.

## Layout

```
tickers/
├── docs/DESIGN.md              # architecture & design document
├── DEPLOYMENT.md               # deploying, upgrading, backing up
├── legacy/                     # the original cron script, kept for reference
├── scripts/                    #   quickstart.sh · build.sh · version.sh
├── deploy/                     #   reference systemd unit
└── server/                     # the Go application — ONE static binary
    ├── cmd/tickers/            #   entrypoint & CLI
    └── internal/
        ├── store/              #   SQLite: schema, migrations, every query
        ├── quotes/             #   quote providers (Yahoo Finance)
        ├── publish/            #   downstream publishing + the legacy payload
        ├── engine/             #   the scheduled refresh + publish cycle
        ├── api/                #   the REST layer
        └── web/assets/         #   the web client, embedded at build time
```

The deployable artifact is a **single static Go binary**
(`server/bin/tickers`) that serves the REST API and the web client from one
origin, with zero runtime dependencies.

## Getting started

```bash
scripts/build.sh                              # needs Go >= 1.21 (newer toolchains fetch automatically)
./server/bin/tickers serve --db ./data/tickers.sqlite
```

Open `http://localhost:8797`. The watchlist starts with the placeholders; press
**Replace** on each, or **Add** your own. To use it from your phone, reach the
server over your LAN/Tailscale and "Add to Home Screen".

While working on the web client, skip the rebuild — serve the assets from disk:

```bash
go run ./server/cmd/tickers serve --web-dist server/internal/web/assets
```

### Quick start on a Raspberry Pi (or any Linux box)

Install Tickers as a hardened **systemd service** with one command:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/tickers/main/scripts/quickstart.sh | sudo bash
```

(or, from a checkout: `sudo ./scripts/quickstart.sh`)

It installs Go if needed (build-time only), creates a dedicated `tickers`
system user, compiles the static binary, and runs it under systemd serving the
API + web client on `http://<host>:8797`.

**Or skip the build entirely** and install the prebuilt binary from the latest
[release](https://github.com/chinmay28/tickers/releases) — no Go, no source
tree, seconds instead of minutes on a Pi:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/tickers/main/scripts/quickstart.sh \
  | sudo TICKERS_INSTALL=release bash
```

The download's checksum is verified before anything is swapped in, and
`TICKERS_RELEASE=v1.0.42` pins a specific release instead of the latest.
Releases publish **`linux/arm64`** and **`linux/amd64`**; other architectures
build from source (the default), which works everywhere. Both modes install the
same thing — one static binary with the web client embedded, under the same
unit and the same data directory — so you can switch between them by re-running
with a different `TICKERS_INSTALL`.

> Tickers listens on **8797**, not 8787, so it and CountRoster can share a Pi.

**Re-run it any time to upgrade — installs and upgrades are non-disruptive and
never lose data:**

- The live SQLite database lives at a stable path **outside** the source tree
  (`/var/lib/tickers/`), so rebuilding or pulling can't clobber it.
- Each upgrade quiesces the service, **snapshots the database** (`+ WAL/SHM`) to
  a timestamped backup, then swaps code in. The new binary is built (or
  downloaded) to a staging path while the old one keeps serving, so a failed
  build leaves the running app untouched.
- After restart it polls `/api/health`; if the new version is unhealthy it
  **rolls back** to the previous binary and **restores the pre-upgrade
  snapshot**.
- Schema changes run through an append-only, idempotent migration runner, and
  every migration is additive — so the binary it rolls back *to* can still read
  the database the new one touched.
- Re-running never re-seeds: placeholders you replaced stay replaced, and
  symbols you deleted stay deleted.

Override defaults with env vars (`PORT`, `HOST`, `TICKERS_INSTALL`,
`TICKERS_REF`, `TICKERS_RELEASE`, `TICKERS_DATA_DIR`, `TICKERS_PREFIX`,
`TICKERS_USER`, …). The generated unit is documented at
[`deploy/tickers.service`](./deploy/tickers.service). Manage it with
`systemctl status tickers` and `journalctl -u tickers -f`.

## Using it

**Watchlist** — the symbols being tracked. Each row shows the last price, the
move from the previous close, a sparkline from stored history, and when it was
last read. Placeholders carry a `placeholder` chip until you replace them.
Drag to reorder on a desktop, or use ↑↓ on a phone; **the watchlist order is
the payload order**.

**Publishing** — where snapshots go. A destination is a base URL, a key, an
optional category, and a format. After every refresh the snapshot is
`PUT {base}/{key}`; if that fails (typically a 404 because the entry doesn't
exist yet) it is `POST {base}`. The page shows a live preview of exactly what a
destination receives, and **Test** sends the real payload to one destination on
demand.

Two formats:

| Format | Payload |
|---|---|
| `minion` (default) | `{"VTI": "295.50", "BTC-USD": "N/A", "timestamp": "08/07 14:03:22"}` — the original script's shape, for existing consumers |
| `detailed` | per-symbol objects with `price`, `previousClose`, `change`, `changePercent`, `currency`, `status`, plus an ISO timestamp |

**Activity** — the last refresh cycles, with per-symbol counts, which verb each
destination accepted, and the failures in full.

**Settings** — two groups, both live:

- *Refresh loop* — how often symbols are fetched (a seconds field plus
  30s/1m/5m/15m/1h presets; 30s is the floor), how long price history is kept,
  and whether every refresh also publishes.
- *Quote source* — the **server URL** prices come from, the request timeout,
  and the User-Agent sent upstream. Leave a field blank to fall back to the
  default, which the field shows as its placeholder. **Test connection** fetches
  one symbol right then and reports the price or the exact error — the fastest
  way to tell a wrong URL from a blocked network from a bad symbol.

Below those, a read-only **Server** card shows what the process was started
with: listen address, database path, whether the client is embedded or served
from disk, and the quote settings actually in force.

Everything in Settings takes effect on the next cycle. Nothing needs a restart.

## Configuration

Almost everything is configured in the GUI and stored in the database: the
watchlist, the publish destinations, the poll interval, and the quote source.
The flags below are what has to be decided before the process starts.

| Flag | Env fallback | Default | Meaning |
|---|---|---|---|
| `--port` | `PORT` | `8797` | listen port |
| `--host` | `HOST` | `0.0.0.0` | bind address |
| `--db` | `TICKERS_DB` | `./data/tickers.sqlite` | SQLite file path |
| `--web-dist` | `WEB_DIST` | — | serve the client from this directory instead of the embedded copy |
| `--verbose` | `TICKERS_VERBOSE` | off | log every API request |
| `--quote-base-url` | `TICKERS_QUOTE_BASE_URL` | Yahoo's | quote API root — *overridable in the GUI* |
| `--quote-timeout` | `TICKERS_QUOTE_TIMEOUT` | `20` | seconds per quote request — *overridable in the GUI* |
| `--quote-user-agent` | `TICKERS_QUOTE_USER_AGENT` | a browser string | *overridable in the GUI* |

For the first five: **flag > env > default**.

The three quote-source flags are a *fallback*, not a fixed value — they exist so
a systemd unit can be templated, but the Settings page wins:
**stored setting > flag > env > built-in default**. Clearing the field in the
GUI reveals the flag again; clearing both reveals the built-in default.

The listen address and database path deliberately stay out of the GUI: a web
app that can change the port it is served on is a web app that can lock you out
of itself. They're shown read-only on the Settings page so you don't have to go
read the unit file to find them.

There is also a one-shot mode, which is the original script's job exactly:

```bash
tickers publish --db /var/lib/tickers/tickers.sqlite   # fetch, publish, exit
```

Useful from cron on a host that would rather not run a daemon. It exits
non-zero if every symbol failed.

## Versioning

The same scheme CountRoster uses: **`vMAJOR.MINOR.PATCH`, where the patch
number is the repository's commit count** — every commit is a patch release,
so `v1.0.42` is the 42nd commit on the 1.0 line. It's shown in the app header,
printed by `tickers version`, and returned by `/api/health`.

Tickers starts on **1.0**. Major and minor are constants in
[`server/internal/version/version.go`](./server/internal/version/version.go)
and are bumped by hand; `scripts/version.sh` is the one place that assembles
the whole string, reading those constants so nothing can disagree about them.

```bash
scripts/version.sh            # v1.0.42
scripts/version.sh --patch    # 42
```

A build with no git — a tarball, or a **shallow clone** — reports patch `0`.
That's deliberate: `git clone --depth 1` answers `rev-list --count HEAD` with
`1`, which isn't an error and isn't obviously wrong, it just quietly ships a
build calling itself `v1.0.1`. Patch `0` is the agreed "unstamped development
build" marker, it matches the Go default, and the release workflow refuses to
publish one. Anything building a release needs the full commit graph:
`fetch-depth: 0` in Actions, and `--filter=blob:none` rather than `--depth 1`
for a cheap clone that still carries all of it (which is what
`scripts/quickstart.sh` does, deepening an old shallow checkout if it finds
one).

Because the tag is determined by the commit rather than chosen, releasing is:

```bash
git tag "$(scripts/version.sh)" && git push origin "$(scripts/version.sh)"
```

The workflow refuses to publish if the tag doesn't match the version the commit
builds.

## Testing & checks

```bash
cd server
go vet ./...
go test -race ./...     # store, quote parsing, publishing, the engine, the API
```

The suites under `server/internal/` are the authority on behaviour. In
particular `internal/publish` pins the legacy payload: if a change would alter
what an existing consumer receives, those tests fail.

## Documentation

- [docs/DESIGN.md](./docs/DESIGN.md) — architecture, schema, the REST contract
- [DEPLOYMENT.md](./DEPLOYMENT.md) — deploying, upgrading, backup and restore
- [server/README.md](./server/README.md) — the Go server and its CLI
- [CHANGELOG.md](./CHANGELOG.md)

## Credits

Built by **CM Hegday** ([github.com/chinmay28](https://github.com/chinmay28)) —
tap the developer mark in the app header to see the badge.

## License

Tickers is free software licensed under the **GNU Affero General Public License
v3.0** (`AGPL-3.0-only`). See [LICENSE](./LICENSE) for the full text.

The AGPL is a strong copyleft license: anyone who distributes Tickers — or
**runs a modified version as a network service** — must make the complete
corresponding source available under the same license.

> **Note for operators (AGPL §13):** if you run a modified Tickers server that
> other people interact with over a network, you must offer those users the
> corresponding source of your modified version.

Market data comes from Yahoo Finance's public endpoints. It is provided for
personal, informational use; it is not a licensed market-data feed, and it
carries no warranty of accuracy or timeliness. Don't trade on it.
