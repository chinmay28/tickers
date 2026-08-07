# Deploying Tickers

Everything here assumes a Linux host on a network you trust — a Raspberry Pi on
your LAN, a small VM on a Tailscale tailnet. **Tickers has no authentication by
design.** Anyone who can reach the port can change your watchlist and your
publish destinations, so don't put it on the open internet without putting
something in front of it (§5).

## 1. Install

### The quick start (recommended)

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/tickers/main/scripts/quickstart.sh | sudo bash
```

That builds from source. On a Raspberry Pi that takes a couple of minutes, most
of it compiling SQLite. To skip the build and install a prebuilt binary:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/tickers/main/scripts/quickstart.sh \
  | sudo TICKERS_INSTALL=release bash
```

Either way you end up with the same thing:

| | |
|---|---|
| Binary | `/opt/tickers/src/server/bin/tickers` (source) or `/opt/tickers/bin/tickers` (release) |
| Database | `/var/lib/tickers/tickers.sqlite` |
| Backups | `/var/lib/tickers/backups/` |
| Service | `tickers.service`, running as the `tickers` system user |
| URL | `http://<host>:8797` |

Environment variables, all optional:

| Variable | Default | |
|---|---|---|
| `TICKERS_INSTALL` | `source` | `source` or `release` |
| `TICKERS_REPO` | this repo | clone a fork instead |
| `TICKERS_REF` | `main` | branch, tag or commit (source mode) |
| `TICKERS_RELEASE` | `latest` | pin a release tag (release mode) |
| `TICKERS_USER` | `tickers` | service account |
| `TICKERS_PREFIX` | `/opt/tickers` | install prefix |
| `TICKERS_DATA_DIR` | `/var/lib/tickers` | database + backups |
| `PORT` / `HOST` | `8797` / `0.0.0.0` | listen address |
| `INSTALL_GO` | `auto` | `never` to refuse installing Go |
| `BACKUP_KEEP` | `10` | pre-upgrade snapshots retained |

### By hand

```bash
git clone https://github.com/chinmay28/tickers.git && cd tickers
scripts/build.sh
sudo install -m755 server/bin/tickers /usr/local/bin/tickers
sudo install -d -o tickers -g tickers -m750 /var/lib/tickers
sudo cp deploy/tickers.service /etc/systemd/system/     # edit the paths first
sudo systemctl daemon-reload && sudo systemctl enable --now tickers
```

`deploy/tickers.service` is the reference unit; the quick start writes a copy
with your real paths substituted.

### Running two apps on one Pi

Tickers listens on **8797** and CountRoster on **8787**, with separate users,
separate data directories and separate units, so they coexist without any
configuration. If you run something else on 8797, set `PORT` before installing.

## 2. Upgrading

Re-run the same command:

```bash
curl -fsSL https://raw.githubusercontent.com/chinmay28/tickers/main/scripts/quickstart.sh | sudo bash
```

The script is idempotent and knows it is upgrading. In order it:

1. **Detects the upgrade** before touching anything (an existing database or
   unit file).
2. **Builds or downloads to a staging path** while the old version keeps
   serving. A failed compile or a bad download stops here, and the running
   service never noticed.
3. **Smoke-tests the new binary** with `tickers version` — no database, no
   port — which catches a wrong-architecture download.
4. **Stops the service, then snapshots the database** (plus `-wal`/`-shm`) to
   `backups/tickers-YYYYmmdd-HHMMSS.sqlite`. Stopping first is the point: a
   snapshot taken while writers are live is a snapshot of a half-written WAL.
5. **Swaps the binary in**, keeping the old one as `tickers.prev`.
6. **Restarts and polls `/api/health`** for 15 seconds.
7. **Rolls back if that fails** — the previous binary goes back, the pre-upgrade
   snapshot is restored, the source tree is rewound to the previous commit, and
   the service restarts. You get a non-zero exit and a message saying so.

Schema changes are applied on startup by an append-only, idempotent migration
runner. Every migration is additive, which is what makes step 7 safe: the older
binary can still read a database the newer one has already migrated.

**Nothing is re-seeded on an upgrade.** Placeholders you replaced stay replaced,
symbols you deleted stay deleted, and your destinations and settings are
untouched.

### Pinning and rolling back on purpose

```bash
# install a specific release
curl -fsSL …/quickstart.sh | sudo TICKERS_INSTALL=release TICKERS_RELEASE=v1.0.42 bash

# build a specific branch or commit
curl -fsSL …/quickstart.sh | sudo TICKERS_REF=some-branch bash
```

To go back a version by hand, install the older release and restore the
matching snapshot (§3).

## 3. Backup and restore

The database is the only state. Three ways to get a copy:

```bash
# 1. The pre-upgrade snapshots the quick start already takes
ls -lt /var/lib/tickers/backups/

# 2. A consistent copy of a running instance
sudo -u tickers sqlite3 /var/lib/tickers/tickers.sqlite ".backup '/tmp/tickers.sqlite'"

# 3. Stop it and copy the file (take the sidecars too)
sudo systemctl stop tickers
sudo cp /var/lib/tickers/tickers.sqlite* /somewhere/safe/
sudo systemctl start tickers
```

Restoring:

```bash
sudo systemctl stop tickers
sudo cp /var/lib/tickers/backups/tickers-20260807-141500.sqlite /var/lib/tickers/tickers.sqlite
# copy the -wal/-shm sidecars if the snapshot has them, and delete them if it doesn't
sudo rm -f /var/lib/tickers/tickers.sqlite-wal /var/lib/tickers/tickers.sqlite-shm
sudo chown tickers:tickers /var/lib/tickers/tickers.sqlite*
sudo systemctl start tickers
```

Snapshots older than `BACKUP_KEEP` (default 10) are pruned on each upgrade. If
you want them off-box, rsync the `backups/` directory somewhere — they are
plain SQLite files.

## 4. Operating

```bash
systemctl status tickers
journalctl -u tickers -f
curl -s localhost:8797/api/health | jq
```

`/api/health` returns the version, uptime and the applied migration list, and
answers `503` if the database is unreachable — which is what the quick start's
rollback keys off, so don't make it unconditionally `200`.

**Verbose logging.** Add `--verbose` to `ExecStart` (or set
`TICKERS_VERBOSE=1`) to log every API request. Off by default; the refresh loop
logs one line per cycle either way.

**Nothing is being published.** Check, in order: is at least one destination
**enabled** on the Publishing tab; is *Publish after every refresh* on in
Settings; and press **Test** on the destination — it sends the real payload and
reports the exact HTTP response.

**Every symbol shows N/A.** The host can't reach Yahoo's endpoints. Check
`journalctl -u tickers` for the error on each symbol; a corporate proxy or a
DNS-filtering router is the usual cause. `curl -s -o /dev/null -w '%{http_code}'
'https://query1.finance.yahoo.com/v8/finance/chart/VTI?range=1d&interval=1m'`
from the host tells you quickly.

**One symbol shows N/A.** Usually a typo or a symbol Yahoo doesn't carry under
that name. The row shows the provider's own error; use **Search by name** to
find the right ticker.

**Rate limiting.** The poll interval floor is 30 seconds and requests are capped
at four in flight. If you are watching dozens of symbols and seeing failures,
raise the interval before anything else.

## 5. Exposure and TLS

The simplest safe setup is [Tailscale](https://tailscale.com):

```bash
sudo tailscale serve --bg 8797     # HTTPS on your tailnet, no ports opened
```

That gives you a real certificate, which is also what "Add to Home Screen"
wants. Behind a reverse proxy instead:

```nginx
location / {
    proxy_pass http://127.0.0.1:8797;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;
}
```

or, with Caddy:

```
tickers.example.com {
    reverse_proxy 127.0.0.1:8797
}
```

If you expose it beyond a trusted network, put authentication in the proxy
(basic auth, an identity-aware proxy, a VPN). The application has none and will
not gain any — see DESIGN.md.

Bind to loopback when a proxy is in front: set `HOST=127.0.0.1` so the port
isn't reachable directly.

## 6. Uninstalling

```bash
sudo systemctl disable --now tickers
sudo rm /etc/systemd/system/tickers.service && sudo systemctl daemon-reload
sudo rm -rf /opt/tickers
sudo userdel tickers
# the data survives on purpose — delete it deliberately:
sudo rm -rf /var/lib/tickers
```
