# Changelog

Versions are `vMAJOR.MINOR.PATCH` where the patch number is the repository's
commit count (see [README](./README.md#versioning)). The release workflow reads
the section matching a tag out of this file and uses it as the release body, so
each heading must be `## <tag> — <title>`.

## Unreleased

### The script becomes an application

Tickers is `update_minion_quotes.py` — a cron script that fetched seven
hardcoded symbols and PUT them into a key-value store — rebuilt as a
self-hosted web app in the shape of
[CountRoster](https://github.com/chinmay28/countroster).

**What you can now do without editing source:**

- Add, replace, relabel, pause, reorder and remove symbols from a browser. The
  seven symbols the script hardcoded ship as *placeholders* with a one-click
  **Replace** action.
- Search Yahoo by company name when you don't know the ticker.
- Publish to several destinations, each with its own base URL, key, category,
  format and timeout — and **Test** one on demand with the real payload.
- Change the poll interval (a seconds field plus 30s/1m/5m/15m/1h presets),
  history retention, and whether every refresh publishes.
- Change the **quote source** — the upstream server URL, the request timeout
  and the User-Agent — and press **Test connection** to fetch one symbol and
  see the price or the exact error. The user-agent field matters in practice:
  Yahoo stonewalls obvious scripts and the string that works drifts, so this
  turns a redeploy into a text box. A read-only *Server* card shows the
  start-up configuration (listen address, database path, client source) that a
  browser genuinely can't change.
- Everything in Settings takes effect on the next cycle; no restart. The
  quote-source fields also exist as `--quote-*` flags for templating a systemd
  unit, and the ordering is stored setting > flag > env > built-in default.
- See recent refresh cycles on an Activity page, with per-symbol counts, which
  verb each destination accepted, and the failures in full. The server retains
  the last 500.

**What deliberately did not change.** The published payload is byte-for-byte
what the script wrote — flat map of symbol → 2-decimal string, `"N/A"` for a
failure, a `"timestamp"` in `MM/DD HH:MM:SS` — sent with the same
PUT-then-POST fallback, and the price is still the last non-null close of the
1-minute intraday series. Existing consumers need no changes. A new `detailed`
format is available per destination for anyone who'd rather not parse strings.

**How it runs.** One static Go binary with the web client embedded via
`go:embed` and a pure-Go SQLite driver — no Python, no Node, no runtime
dependencies, and it cross-compiles to a Raspberry Pi.

- `scripts/quickstart.sh` installs it as a hardened systemd service from source
  or from a prebuilt release, and re-running it upgrades in place: it stages
  the new binary while the old one serves, snapshots the database once the
  service is quiesced, health-checks the result, and rolls back — binary and
  data — if the new version is unhealthy.
- Schema changes go through an append-only, idempotent, additive-only migration
  runner, which is what makes that rollback safe.
- Listens on **8797** so it can share a Pi with CountRoster.

**Also:** `tickers publish` runs one cycle and exits, for anyone who'd rather
keep using cron than run a daemon.
