# The original script

[`update_minion_quotes.py`](./update_minion_quotes.py) is what Tickers grew out
of: a cron script that fetched seven hardcoded symbols with `yfinance` and PUT
them into a home-automation key-value store, falling back to POST.

It is kept here unmodified, as the specification for the parts that must not
change. Four things in it are contracts, and the Go implementation reproduces
all four — see [docs/DESIGN.md](../docs/DESIGN.md#where-it-came-from):

1. The payload shape — `{"VTI": "295.50", …, "timestamp": "08/07 14:03:22"}`,
   with `"N/A"` for an unavailable price.
2. The PUT-then-POST fallback.
3. The price: the last non-null close of the 1-minute intraday series, not the
   quote endpoint's `regularMarketPrice`.
4. Never failing loudly — one bad symbol doesn't stop the publish.

`internal/publish/publish_test.go` is where those are pinned as tests.

## Migrating from it

Nothing downstream needs to change. Install Tickers (see the
[README](../README.md)), then on the **Publishing** tab add a destination with:

| Field | Value from the script |
|---|---|
| Base URL | the `base_url` — e.g. `http://100.84.70.60:9999/api/entries` |
| Key | the `key` — e.g. `minion-quotes` |
| Category | `minion` |
| Format | `minion` (the default) |

Then replace the placeholder watchlist with whatever you actually want to
track, and disable the cron entry that ran the script.

If you'd rather keep using cron than run a daemon, the one-shot mode does the
script's job exactly:

```bash
tickers publish --db /var/lib/tickers/tickers.sqlite
```
