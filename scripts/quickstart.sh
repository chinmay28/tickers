#!/usr/bin/env bash
#
# Tickers — Linux quick-start installer (Ubuntu / Debian / Raspberry Pi OS).
#
# One command, run as root, installs Tickers as a hardened systemd service:
#
#   curl -fsSL https://raw.githubusercontent.com/chinmay28/tickers/main/scripts/quickstart.sh | sudo bash
#
# Two ways to get the binary — TICKERS_INSTALL picks one:
#
#   source   (default) clone the repo and build it here. Needs Go at build time
#            (installed automatically if missing); works on any architecture
#            and can track any branch/tag/commit. There is NO Node step — the
#            web client is hand-written and embedded with go:embed, so a Go
#            toolchain is the entire build.
#   release  download the prebuilt static binary from a GitHub release. No
#            toolchain, no source tree, no compile — seconds instead of minutes
#            on a Raspberry Pi. Only architectures the release publishes are
#            supported; anything else should use source.
#
#            curl -fsSL …/quickstart.sh | sudo TICKERS_INSTALL=release bash
#
# Both modes produce the same thing: one static binary with the web client
# embedded, run by the same systemd unit, with the same data directory. You can
# switch between them by re-running with a different TICKERS_INSTALL.
#
# It is deliberately *non-disruptive* and *data-safe* — re-run it any time to
# upgrade in place:
#
#   * Idempotent. Re-running only swaps in newer code; it never re-initialises
#     data and never resurrects the seeded placeholder watchlist.
#   * The live SQLite database lives at a stable path OUTSIDE the source tree
#     ($DATA_DIR), so cloning, rebuilding or pulling can never clobber it.
#   * Every upgrade STOPS the service, snapshots the database (+ WAL/SHM
#     sidecars) to a timestamped backup, THEN swaps code in — so the backup is
#     always taken against a quiesced database.
#   * The new build is compiled (or the new binary downloaded) while the old
#     version keeps serving. If that fails, the running service is untouched.
#   * After restart we poll /api/health; if the new version is unhealthy we ROLL
#     BACK to the previous commit (source mode) or the previous binary (release
#     mode), restore the pre-upgrade database snapshot, and restart — so a bad
#     upgrade self-heals to the last good state with its data.
#   * Schema changes are applied by the server's append-only, idempotent
#     migration runner on startup (additive only; older data stays readable, so
#     a rolled-back binary can still read a database a newer one has touched).
#
# Configure via environment variables (all optional):
#
#   TICKERS_INSTALL   source | release        where the binary comes from (default: source)
#   TICKERS_REPO      git URL to clone        (default: https://github.com/chinmay28/tickers.git)
#   TICKERS_REF       branch/tag/commit       (default: main; source mode)
#   TICKERS_RELEASE   latest | <tag>          release to install (default: latest; release mode)
#   TICKERS_USER      service system user     (default: tickers)
#   TICKERS_PREFIX    install prefix          (default: /opt/tickers; source → $PREFIX/src)
#   TICKERS_DATA_DIR  database + backups dir  (default: /var/lib/tickers)
#   PORT              port to listen on       (default: 8797)
#   HOST              bind address            (default: 0.0.0.0)
#   INSTALL_GO        auto | never            install Go if missing/old (default: auto; build-time only)
#   BACKUP_KEEP       pre-upgrade backups kept (default: 10)
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
if [ -t 1 ]; then
  C_BLUE=$'\033[1;34m'; C_GREEN=$'\033[1;32m'; C_YELLOW=$'\033[1;33m'
  C_RED=$'\033[1;31m'; C_DIM=$'\033[2m'; C_OFF=$'\033[0m'
else
  C_BLUE=''; C_GREEN=''; C_YELLOW=''; C_RED=''; C_DIM=''; C_OFF=''
fi
log()  { printf '%s==>%s %s\n' "$C_BLUE" "$C_OFF" "$*"; }
ok()   { printf '%s ok %s %s\n' "$C_GREEN" "$C_OFF" "$*"; }
warn() { printf '%swarn%s %s\n' "$C_YELLOW" "$C_OFF" "$*" >&2; }
die()  { printf '%serr %s %s\n' "$C_RED" "$C_OFF" "$*" >&2; exit 1; }
step() { printf '\n%s%s%s\n' "$C_DIM" "$*" "$C_OFF"; }

# ---------------------------------------------------------------------------
# Must be root (system-wide service + dedicated user)
# ---------------------------------------------------------------------------
if [ "$(id -u)" -ne 0 ]; then
  die "Run as root: curl -fsSL .../quickstart.sh | sudo bash   (or: sudo ./scripts/quickstart.sh)"
fi
command -v systemctl >/dev/null 2>&1 || die "systemd is required (no systemctl found)."

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
INSTALL_MODE="${TICKERS_INSTALL:-source}"
case "$INSTALL_MODE" in
  source | release) ;;
  *) die "TICKERS_INSTALL must be 'source' or 'release' (got '$INSTALL_MODE')." ;;
esac
TICKERS_REPO="${TICKERS_REPO:-https://github.com/chinmay28/tickers.git}"
TICKERS_REF="${TICKERS_REF:-main}"
RELEASE_TAG="${TICKERS_RELEASE:-latest}"
SVC_USER="${TICKERS_USER:-tickers}"
PREFIX="${TICKERS_PREFIX:-/opt/tickers}"
DATA_DIR="${TICKERS_DATA_DIR:-/var/lib/tickers}"
# 8797, not 8787: CountRoster owns 8787, and the two are meant to coexist on
# the same Raspberry Pi.
PORT="${PORT:-8797}"
HOST="${HOST:-0.0.0.0}"
INSTALL_GO="${INSTALL_GO:-auto}"
BACKUP_KEEP="${BACKUP_KEEP:-10}"

SRC_DIR="$PREFIX/src"
DB_PATH="$DATA_DIR/tickers.sqlite"
BACKUP_DIR="$DATA_DIR/backups"
SERVICE_NAME="tickers"
UNIT_PATH="/etc/systemd/system/${SERVICE_NAME}.service"
# Minimum Go release that can bootstrap the build; the go directive in
# server/go.mod pins the real toolchain, which Go fetches automatically.
GO_MIN_MINOR=21
GO_INSTALL_VERSION="1.25.0"

# If this script is being run from inside an existing checkout (sudo ./scripts/
# quickstart.sh) rather than piped from curl, build that checkout in place.
# Release mode never builds, so it ignores the surrounding checkout entirely.
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" >/dev/null 2>&1 && pwd)"
LOCAL_CHECKOUT=""
if [ "$INSTALL_MODE" = source ] && git -C "$SELF_DIR" rev-parse --show-toplevel >/dev/null 2>&1; then
  top="$(git -C "$SELF_DIR" rev-parse --show-toplevel)"
  if [ -f "$top/server/go.mod" ] && grep -q 'module github.com/chinmay28/tickers/server' "$top/server/go.mod" 2>/dev/null; then
    LOCAL_CHECKOUT="$top"
    SRC_DIR="$top"   # build & serve from where the user already cloned
  fi
fi

if [ "$INSTALL_MODE" = release ]; then
  # No source tree at all: the binary is the whole install.
  SERVER_BIN="$PREFIX/bin/tickers"
  WORK_DIR="$PREFIX"
else
  SERVER_BIN="$SRC_DIR/server/bin/tickers"
  WORK_DIR="$SRC_DIR"
fi
# Kept for rollback: the binary the service was running before this run.
PREV_BIN="${SERVER_BIN}.prev"
STAGED_BIN="${SERVER_BIN}.new"

log "Tickers quick start"
printf '  %-10s %s\n' "install"  "$INSTALL_MODE$( [ "$INSTALL_MODE" = release ] && echo " ($RELEASE_TAG)" )"
if [ "$INSTALL_MODE" = release ]; then
  printf '  %-10s %s\n' "binary"  "$SERVER_BIN"
else
  printf '  %-10s %s\n' "source"  "$SRC_DIR"
fi
printf '  %-10s %s\n' "data"     "$DATA_DIR"
printf '  %-10s %s\n' "database" "$DB_PATH"
printf '  %-10s %s\n' "service"  "${SERVICE_NAME}.service (user: $SVC_USER)"
printf '  %-10s %s\n' "listen"   "http://$HOST:$PORT"

# Run git/go as the service user so the tree stays owned by them, and so the
# build matches the runtime account. Falls back to plain exec before the user
# exists.
as_svc() {
  if id -u "$SVC_USER" >/dev/null 2>&1; then
    sudo -u "$SVC_USER" --preserve-env=PATH "$@"
  else
    "$@"
  fi
}

# ---------------------------------------------------------------------------
# 1. Prerequisites: curl always; git + Go only when building from source
# ---------------------------------------------------------------------------
step "[1/7] Prerequisites"

APT=0; command -v apt-get >/dev/null 2>&1 && APT=1
ensure_pkg() {
  command -v "$1" >/dev/null 2>&1 && return 0
  [ "$APT" -eq 1 ] || die "'$1' missing and no apt-get to install it. Install it and re-run."
  log "installing $1…"; apt-get update -y >/dev/null; apt-get install -y "$1" >/dev/null
}
ensure_pkg curl

GO_DIR=""
if [ "$INSTALL_MODE" = release ]; then
  # Nothing is compiled and nothing is cloned: curl (to fetch the release) and
  # sha256sum (to check it) are the whole toolchain.
  command -v sha256sum >/dev/null 2>&1 || ensure_pkg coreutils
  ok "curl present — release mode needs no git or Go"
else
  ensure_pkg git
  ok "git $(git --version | awk '{print $3}'), curl present"

  go_ok=0
  GO_BIN="$(command -v go || true)"
  [ -z "$GO_BIN" ] && [ -x /usr/local/go/bin/go ] && GO_BIN=/usr/local/go/bin/go
  if [ -n "$GO_BIN" ]; then
    go_minor="$("$GO_BIN" env GOVERSION 2>/dev/null | sed -E 's/^go1\.([0-9]+).*/\1/' || echo 0)"
    [ "${go_minor:-0}" -ge "$GO_MIN_MINOR" ] && go_ok=1
  fi
  if [ "$go_ok" -eq 1 ]; then
    ok "$("$GO_BIN" version | awk '{print $3}') (newer toolchains fetch automatically per go.mod)"
  else
    [ -n "$GO_BIN" ] \
      && warn "$("$GO_BIN" version 2>/dev/null | awk '{print $3}') is too old; Tickers needs Go >= 1.$GO_MIN_MINOR." \
      || warn "Go not found (needed to build the server binary)."
    [ "$INSTALL_GO" = never ] && die "Install Go >= 1.$GO_MIN_MINOR (https://go.dev/dl) and re-run, or set INSTALL_GO=auto."
    case "$(uname -m)" in
      x86_64)          go_arch=amd64 ;;
      aarch64 | arm64) go_arch=arm64 ;;
      armv6l | armv7l) go_arch=armv6l ;;
      *) die "Unsupported architecture $(uname -m) for automatic Go install; install Go manually and re-run." ;;
    esac
    log "installing Go $GO_INSTALL_VERSION ($go_arch) to /usr/local/go…"
    curl -fsSL "https://go.dev/dl/go${GO_INSTALL_VERSION}.linux-${go_arch}.tar.gz" -o /tmp/tickers-go.tgz
    rm -rf /usr/local/go
    tar -C /usr/local -xzf /tmp/tickers-go.tgz
    rm -f /tmp/tickers-go.tgz
    GO_BIN=/usr/local/go/bin/go
    ok "$("$GO_BIN" version | awk '{print $3}') installed"
  fi
  GO_DIR="$(dirname "$GO_BIN")"
fi

# ---------------------------------------------------------------------------
# 2. Dedicated system user (home = data dir, no login shell)
# ---------------------------------------------------------------------------
step "[2/7] Service user '$SVC_USER'"
if id -u "$SVC_USER" >/dev/null 2>&1; then
  ok "user '$SVC_USER' already exists"
else
  nologin="$(command -v nologin || echo /usr/sbin/nologin)"
  useradd --system --home-dir "$DATA_DIR" --create-home --shell "$nologin" "$SVC_USER"
  ok "created system user '$SVC_USER'"
fi

# ---------------------------------------------------------------------------
# 3. The code: a release binary to download, or a source tree to clone/update.
#    Either way the data directory is elsewhere and is never touched here.
# ---------------------------------------------------------------------------

# The architecture name in the asset. Releases publish only what the release
# workflow builds (GOARCHES in .github/workflows/release.yml) — anything else
# has to build from source, which works everywhere.
release_arch() {
  case "$(uname -m)" in
    aarch64 | arm64) echo arm64 ;;
    x86_64 | amd64)  echo amd64 ;;
    *) die "no prebuilt binary for $(uname -m); re-run with TICKERS_INSTALL=source to build one." ;;
  esac
}

# owner/repo, derived from TICKERS_REPO so a fork's releases are found too.
release_slug() {
  printf '%s' "$TICKERS_REPO" | sed -E 's#^.*github\.com[:/]+##; s#\.git$##; s#/+$##'
}

release_url() {
  local slug; slug="$(release_slug)"
  if [ "$RELEASE_TAG" = latest ]; then
    printf 'https://github.com/%s/releases/latest/download/%s' "$slug" "$1"
  else
    printf 'https://github.com/%s/releases/download/%s/%s' "$slug" "$RELEASE_TAG" "$1"
  fi
}

RELEASE_VERSION=""
fetch_release() {
  local arch asset url tmp
  arch="$(release_arch)"
  asset="tickers-linux-$arch"
  url="$(release_url "$asset")"

  install -d -m 755 "$PREFIX" "$(dirname "$SERVER_BIN")"
  tmp="$(mktemp -d)"

  log "downloading $asset ($RELEASE_TAG) from $(release_slug)…"
  curl -fSL --progress-bar --retry 3 --retry-delay 2 -o "$tmp/$asset" "$url" \
    || die "could not download $url — no release '$RELEASE_TAG' publishes linux/$arch yet? Re-run with TICKERS_INSTALL=source."

  # Verify the checksum published beside the binary. A missing .sha256 is a
  # warning (older releases predate it); a mismatch is fatal.
  if curl -fsL --retry 2 -o "$tmp/$asset.sha256" "$url.sha256"; then
    (cd "$tmp" && sha256sum -c "$asset.sha256" >/dev/null 2>&1) \
      || die "checksum mismatch on $asset — refusing to install it."
    ok "sha256 verified"
  else
    warn "no $asset.sha256 published — installing without checksum verification."
  fi

  chmod 755 "$tmp/$asset"
  # Cheapest possible smoke test, and it catches a wrong-architecture download
  # before anything is swapped in: `version` needs no database and no port.
  RELEASE_VERSION="$("$tmp/$asset" version 2>/dev/null || true)"
  [ -n "$RELEASE_VERSION" ] || die "the downloaded binary does not run on this host (wrong architecture?)."
  mv "$tmp/$asset" "$STAGED_BIN"
  rm -rf "$tmp"
  ok "fetched $RELEASE_VERSION (linux/$arch)"
}

# Swap the staged binary in. `mv` is a rename, so this is safe even while the
# old binary is executing — the running process keeps its own inode.
install_staged() {
  [ -f "$STAGED_BIN" ] || return 0
  if [ -f "$SERVER_BIN" ]; then cp -f "$SERVER_BIN" "$PREV_BIN"; fi
  chown "$SVC_USER":"$SVC_USER" "$STAGED_BIN" 2>/dev/null || true
  chmod 755 "$STAGED_BIN"
  mv -f "$STAGED_BIN" "$SERVER_BIN"
  ok "installed $SERVER_BIN"
}

if [ "$INSTALL_MODE" = release ]; then
  step "[3/7] Release binary ($RELEASE_TAG)"
else
  step "[3/7] Source at $SRC_DIR"
fi

# Detect an upgrade BEFORE we change anything, so we know whether to back up
# and whether a failure should roll back.
UPGRADE=0
{ [ -f "$DB_PATH" ] || [ -f "$UNIT_PATH" ]; } && UPGRADE=1

PREV_SHA=""
if [ "$INSTALL_MODE" = release ]; then
  # Downloaded now, installed in step 6 — the old version keeps serving until
  # then, exactly as a source build does while it compiles.
  fetch_release
elif [ -n "$LOCAL_CHECKOUT" ]; then
  warn "building your existing checkout in place (no git fetch)."
  PREV_SHA="$(git -C "$SRC_DIR" rev-parse HEAD 2>/dev/null || true)"
  ok "source at ${PREV_SHA:0:12}"
elif [ -d "$SRC_DIR/.git" ]; then
  PREV_SHA="$(git -C "$SRC_DIR" rev-parse HEAD 2>/dev/null || true)"
  log "updating to $TICKERS_REF…"
  # A shallow checkout would make every build report a patch number equal to
  # its own depth. Deepen it once (scripts/version.sh reports 0 rather than
  # lying, but 0 is not a version anyone wants in production).
  if [ "$(as_svc git -C "$SRC_DIR" rev-parse --is-shallow-repository 2>/dev/null || echo false)" = true ]; then
    log "deepening shallow checkout (the version's patch number is the commit count)…"
    as_svc git -C "$SRC_DIR" fetch --unshallow --filter=blob:none origin \
      || as_svc git -C "$SRC_DIR" fetch --unshallow origin \
      || warn "could not deepen; this build will report patch 0."
  fi
  as_svc git -C "$SRC_DIR" fetch --filter=blob:none origin "$TICKERS_REF" \
    || as_svc git -C "$SRC_DIR" fetch origin "$TICKERS_REF"
  as_svc git -C "$SRC_DIR" checkout -q -B deploy FETCH_HEAD
  ok "updated $( [ -n "$PREV_SHA" ] && echo "${PREV_SHA:0:12} → " )$(git -C "$SRC_DIR" rev-parse --short HEAD)"
else
  log "cloning $TICKERS_REPO (ref: $TICKERS_REF)…"
  mkdir -p "$PREFIX"
  # NOT --depth 1: the version's patch number is the commit count, and a
  # shallow clone would make every build call itself 1.0.1. --filter=blob:none
  # keeps it cheap — the whole commit graph, but only the blobs the checkout
  # actually needs. Fall back for git < 2.19.
  git clone --filter=blob:none --branch "$TICKERS_REF" "$TICKERS_REPO" "$SRC_DIR" \
    || git clone --branch "$TICKERS_REF" "$TICKERS_REPO" "$SRC_DIR" \
    || git clone "$TICKERS_REPO" "$SRC_DIR"
  chown -R "$SVC_USER" "$PREFIX"
  ok "cloned to $SRC_DIR"
fi
if [ "$INSTALL_MODE" = source ]; then
  chown -R "$SVC_USER" "$SRC_DIR" 2>/dev/null || true
  [ -f "$SRC_DIR/server/go.mod" ] || die "no server/go.mod at $SRC_DIR — checkout failed?"
  # git refuses to read a repo owned by someone else; the build runs as the
  # service user, so mark the tree safe for root too (version.sh runs as root).
  git config --global --add safe.directory "$SRC_DIR" 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# 4. Build (the old binary keeps serving while we compile).
#    Release mode has nothing to build — step 3 already fetched the binary.
# ---------------------------------------------------------------------------
if [ "$INSTALL_MODE" = release ]; then
  step "[4/7] Build — skipped (prebuilt release binary)"
else
  step "[4/7] Build (static Go binary with the web client embedded)"
fi

build_src() {
  # Build to the staging path, not over the running binary: a failed compile
  # must leave the service exactly as it was.
  local patch
  patch="$(as_svc "$SRC_DIR/scripts/version.sh" --patch 2>/dev/null || echo 0)"
  install -d -m 755 "$(dirname "$SERVER_BIN")"
  chown "$SVC_USER" "$(dirname "$SERVER_BIN")" 2>/dev/null || true

  as_svc env PATH="$GO_DIR:$PATH" CGO_ENABLED=0 \
    sh -c "cd '$SRC_DIR/server' && go build -trimpath -ldflags '-s -w -X github.com/chinmay28/tickers/server/internal/version.Patch=$patch' -o '$STAGED_BIN' ./cmd/tickers"

  [ -x "$STAGED_BIN" ] || die "build produced no server binary"
  # Same cheap smoke test release mode gets: it needs no database and no port,
  # and it catches a binary that cannot run here before anything is swapped in.
  "$STAGED_BIN" version >/dev/null 2>&1 || die "the freshly built binary does not run on this host."
}

if [ "$INSTALL_MODE" = release ]; then
  ok "nothing to build — $RELEASE_VERSION is staged, installed after the backup"
else
  build_src
  ok "build complete → $("$STAGED_BIN" version) staged, installed after the backup"
fi

# ---------------------------------------------------------------------------
# 5. Data dir + pre-upgrade database snapshot
# ---------------------------------------------------------------------------
step "[5/7] Data directory + backup"
install -d -o "$SVC_USER" -g "$SVC_USER" -m 750 "$DATA_DIR" "$BACKUP_DIR"
ok "data dir ready ($DATA_DIR, owned by $SVC_USER)"

stop_service()  { systemctl stop  "${SERVICE_NAME}.service" 2>/dev/null || true; }
start_service() { systemctl start "${SERVICE_NAME}.service"; }

SNAP=""
if [ "$UPGRADE" -eq 1 ] && [ -f "$DB_PATH" ]; then
  # Quiesce first so the snapshot is consistent (no live WAL writers).
  stop_service
  ts="$(date +%Y%m%d-%H%M%S)"
  SNAP="$BACKUP_DIR/tickers-$ts.sqlite"
  cp "$DB_PATH" "$SNAP"
  for ext in -wal -shm; do [ -f "${DB_PATH}${ext}" ] && cp "${DB_PATH}${ext}" "${SNAP}${ext}"; done
  chown "$SVC_USER":"$SVC_USER" "$SNAP"* 2>/dev/null || true
  ok "database backed up → $SNAP"
  # Prune, keeping the newest $BACKUP_KEEP.
  if [ "$BACKUP_KEEP" -gt 0 ]; then
    ls -1t "$BACKUP_DIR"/tickers-*.sqlite 2>/dev/null | tail -n +"$((BACKUP_KEEP + 1))" | while read -r old; do
      rm -f "$old" "${old}-wal" "${old}-shm"
    done
  fi
fi

# ---------------------------------------------------------------------------
# 6. systemd unit + (re)start
# ---------------------------------------------------------------------------
step "[6/7] systemd service"
# The service is quiesced by now on an upgrade, so this is where the staged
# binary replaces the running one (keeping the old one for rollback).
install_staged

write_unit() {
  cat > "$UNIT_PATH" <<UNIT
[Unit]
Description=Tickers — self-hosted quote watchlist and publisher
Documentation=https://github.com/chinmay28/tickers
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SVC_USER
Group=$SVC_USER
WorkingDirectory=$WORK_DIR
ExecStart=$SERVER_BIN serve --db $DB_PATH --port $PORT --host $HOST
Restart=on-failure
RestartSec=3

# Hardening — safe on a trusted LAN, defensive if exposure ever widens.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=$DATA_DIR

[Install]
WantedBy=multi-user.target
UNIT
}
write_unit
systemctl daemon-reload
systemctl enable "${SERVICE_NAME}.service" >/dev/null 2>&1 || true
start_service
ok "service enabled and started"

# ---------------------------------------------------------------------------
# 7. Health check (with rollback on a failed upgrade)
# ---------------------------------------------------------------------------
step "[7/7] Health check"
health_url="http://127.0.0.1:$PORT/api/health"
check_health() {
  for _ in $(seq 1 30); do
    curl -fsS "$health_url" >/dev/null 2>&1 && return 0
    sleep 0.5
  done
  return 1
}

# Restore the pre-upgrade database snapshot, so the version we roll back to
# sees a schema it understands.
restore_snapshot() {
  if [ -n "$SNAP" ] && [ -f "$SNAP" ]; then
    cp "$SNAP" "$DB_PATH"
    for ext in -wal -shm; do
      if [ -f "${SNAP}${ext}" ]; then cp "${SNAP}${ext}" "${DB_PATH}${ext}"; else rm -f "${DB_PATH}${ext}"; fi
    done
    chown "$SVC_USER":"$SVC_USER" "$DB_PATH"* 2>/dev/null || true
    ok "restored the pre-upgrade database from $SNAP"
  fi
}

if check_health; then
  ok "healthy ($health_url) — $(curl -fsS "$health_url" 2>/dev/null | sed -n 's/.*"version" *: *"\([^"]*\)".*/\1/p')"
elif [ "$UPGRADE" -eq 1 ] && [ -f "$PREV_BIN" ]; then
  # The previous binary is right there in both modes — a source build stages
  # its output the same way a download does — so rollback is the same move:
  # put it back with the pre-upgrade database and restart.
  warn "the new version failed its health check."
  warn "rolling back to the previous binary and restoring the pre-upgrade database…"
  stop_service
  restore_snapshot
  mv -f "$PREV_BIN" "$SERVER_BIN"
  chown "$SVC_USER":"$SVC_USER" "$SERVER_BIN" 2>/dev/null || true
  # Point the source tree back at the commit that binary came from, so the
  # next re-run doesn't rebuild the broken one.
  if [ "$INSTALL_MODE" = source ] && [ -n "$PREV_SHA" ] && [ -z "$LOCAL_CHECKOUT" ]; then
    as_svc git -C "$SRC_DIR" checkout -q -B deploy "$PREV_SHA" || warn "could not rewind the source tree to ${PREV_SHA:0:12}."
  fi
  start_service
  if check_health; then
    die "Upgrade failed its health check — rolled back to $("$SERVER_BIN" version 2>/dev/null || echo "the previous binary") with your data intact. Check: journalctl -u ${SERVICE_NAME} -n 80"
  fi
  die "Upgrade AND rollback both failed health checks. Your data snapshot is safe at ${SNAP:-$DB_PATH}. Inspect: journalctl -u ${SERVICE_NAME} -n 80"
else
  die "Service is not healthy. Inspect logs: journalctl -u ${SERVICE_NAME} -n 80 --no-pager"
fi

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------
lan_ip="$(hostname -I 2>/dev/null | awk '{print $1}')"; [ -n "$lan_ip" ] || lan_ip="<this-host>"
verb="installed"; [ "$UPGRADE" -eq 1 ] && verb="upgraded"

if [ "$INSTALL_MODE" = release ]; then
  origin_line="Installed:   $RELEASE_VERSION, prebuilt from the $RELEASE_TAG release (no toolchain needed)"
  upgrade_line="Upgrade:     re-run with TICKERS_INSTALL=release for the next release."
else
  origin_line="Source:      $SRC_DIR (built here)"
  upgrade_line="Upgrade:     re-run this script — it snapshots data, swaps code in, and self-heals."
fi

cat <<DONE

${C_GREEN}Tickers $verb and running.${C_OFF}

  Open it:     http://$lan_ip:$PORT      (http://localhost:$PORT on this machine)
  Database:    $DB_PATH
  Backups:     $BACKUP_DIR
  Binary:      $SERVER_BIN (static; embeds the web client)
  $origin_line
  $upgrade_line

  First run ships a placeholder watchlist. Open the app, press Replace on each
  one, and add your own — then add a publish destination on the Publishing tab.

  Manage the service:
    systemctl status  ${SERVICE_NAME}
    systemctl restart ${SERVICE_NAME}
    journalctl -u ${SERVICE_NAME} -f
${C_DIM}
  No auth by design — keep this on a trusted network (LAN / Tailscale / VPN).
  For HTTPS + "Add to Home Screen", front it with Tailscale Serve or a reverse
  proxy (Caddy/nginx). See DEPLOYMENT.md.${C_OFF}
DONE
