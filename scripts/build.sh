#!/usr/bin/env bash
#
# Build the deployable artifact: one static Go binary with the web client
# embedded.
#
#   scripts/build.sh                        → server/bin/tickers (this host)
#   GOOS=linux GOARCH=arm64 scripts/build.sh → a Raspberry Pi binary
#   scripts/build.sh -o /tmp/tickers        → somewhere else
#
# There is no front-end build step. The web client is hand-written HTML, CSS
# and one ES module under server/internal/web/assets, embedded with go:embed —
# so this script and a Go toolchain are the entire build.
#
# CGO_ENABLED=0 is load-bearing: the SQLite driver is pure Go, and a static
# binary is what makes cross-compiling to a Pi and shipping one file work.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." >/dev/null 2>&1 && pwd)"
OUT="$ROOT/server/bin/tickers"

while [ $# -gt 0 ]; do
  case "$1" in
    -o | --output) OUT="$2"; shift 2 ;;
    -h | --help) sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "build.sh: unknown argument $1" >&2; exit 1 ;;
  esac
done

PATCH="$("$ROOT/scripts/version.sh" --patch)"
VERSION="$("$ROOT/scripts/version.sh")"

mkdir -p "$(dirname "$OUT")"
echo "==> building $VERSION → $OUT (${GOOS:-$(go env GOOS)}/${GOARCH:-$(go env GOARCH)})"

cd "$ROOT/server"
CGO_ENABLED=0 go build \
  -trimpath \
  -ldflags "-s -w -X github.com/chinmay28/tickers/server/internal/version.Patch=$PATCH" \
  -o "$OUT" \
  ./cmd/tickers

echo "==> $("$OUT" version 2>/dev/null || echo "built") at $OUT"
