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

**Every symbol shows N/A.** Press **Test connection** on the Settings page
first — it fetches one symbol through the current settings and reports the
exact error, which is usually enough on its own. The two common causes:

- *The host can't reach the provider.* A corporate proxy or a DNS-filtering
  router. Confirm from the host with
  `curl -s -o /dev/null -w '%{http_code}' 'https://query1.finance.yahoo.com/v8/finance/chart/VTI?range=1d&interval=1m'`,
  then either fix the network or point **Server URL** (Settings → Quote source)
  at a mirror or caching proxy you can reach.
- *The provider is refusing the client.* Yahoo answers browsers and stonewalls
  obvious scripts, and the string that works drifts over time. Paste a current
  browser User-Agent into Settings → Quote source → **User agent** and test
  again. No restart, no redeploy.

Both fields are stored in the database and override whatever the systemd unit
passed; clearing a field falls back to the unit's value, and clearing both
falls back to the built-in default.

**One symbol shows N/A.** Usually a typo or a symbol Yahoo doesn't carry under
that name. The row shows the provider's own error; use **Search by name** to
find the right ticker.

**Rate limiting.** The poll interval floor is 30 seconds and requests are capped
at four in flight. If you are watching dozens of symbols and seeing failures,
raise the interval (Settings → Refresh loop) before anything else — the presets
go up to an hour.

**Collecting the market-data archive.** Off until a folder is chosen on the
Data page (see the README's *market-data archive* section). On a Pi it belongs
on an external drive. Three one-time steps:

1. **Mount the drive at boot, without making the boot depend on it.** Use
   `nofail`, so a missing drive leaves the Pi booting normally rather than
   dropping to an emergency shell:

   ```bash
   sudo mkdir -p /mnt/usb
   echo 'UUID=<the drive's UUID from `lsblk -f`>  /mnt/usb  ext4  defaults,nofail,noatime  0  2' | sudo tee -a /etc/fstab
   sudo mount -a
   sudo install -d -o tickers -g tickers -m 750 /mnt/usb/tickers-archive
   ```

   Use ext4 (or another Linux filesystem) rather than exFAT or NTFS. SQLite's
   locking and the service user's ownership both need a real Unix filesystem.

2. **Let the service write there.** The unit runs with `ProtectSystem=strict`,
   which makes everything outside the data directory read-only to it. The
   quick start rewrites the unit on every upgrade, so add the permission as a
   drop-in, which survives upgrades:

   ```bash
   sudo mkdir -p /etc/systemd/system/tickers.service.d
   printf '[Service]\nReadWritePaths=-/mnt/usb/tickers-archive\n' |
     sudo tee /etc/systemd/system/tickers.service.d/archive.conf
   sudo systemctl daemon-reload && sudo systemctl restart tickers
   ```

   The leading `-` makes the path optional. Without it, systemd refuses to
   start the service at all while the drive is unplugged, and the watchlist
   would go down with the archive.

3. **Choose the folder.** On the Data page, enter `/mnt/usb/tickers-archive`
   and press *Check folder*. If it says the server cannot write there, step 1's
   ownership or step 2's drop-in is missing. If it warns that the folder is on
   the same disk as the system, the drive isn't mounted. Then press *Start a
   new archive here*.

How it behaves once it is running:

- **Unplugged drive.** The archive shows as unavailable on the Data page and
  collection pauses. The performance sheet and backtests read from Yahoo in
  the meantime, and `/api/health` never looks at the archive. Plug it back in
  and collection resumes within 30 seconds. Nothing is written to the empty
  mount point in between: an archive folder has to carry its marker file to be
  opened, and the app never creates a folder.
- **Disk.** One-minute bars for the whole market are around 60 GB a year.
  Collection pauses when free space drops below the floor set on the Data page
  (10 GB by default). Nothing is ever deleted to make room.
- **Backups.** The pre-upgrade snapshots cover `tickers.sqlite` only. For the
  archive, `rsync` the folder while collection is paused on the Data page. The
  per-year intraday files stop changing once their year is over, so only the
  current year's files and `catalog.sqlite` change between backups. Daily
  history can be refetched; intraday bars older than a source keeps cannot.
- **Moving to a bigger drive.** Mount it, create an empty folder the service
  owns, add it to the drop-in's `ReadWritePaths`, and use *Move the archive
  here* on the Data page. It copies everything, then switches. The old folder
  is left for you to delete.
- **Progress.** The journal gets an `archive progress` line every 15 minutes;
  add `--verbose` for one line per request. `tickers coverage --db
  /var/lib/tickers/tickers.sqlite` prints a summary from the shell.
- **Before the Data page existed** the archive could only be set with
  `--archive`. `TICKERS_ARCHIVE` in the same drop-in still works as a
  fallback, and whatever folder is chosen on the page takes precedence over it.

**Timeouts.** A slow link can need more than the default 20 seconds per
request; Settings → Quote source → **Request timeout** accepts 5–120s. Blank
means the default.

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
not gain any — see [docs/DESIGN.md](./docs/DESIGN.md#threat-model).

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
