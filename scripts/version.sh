#!/usr/bin/env bash
#
# The one place the app's version number is assembled.
#
# Scheme: vMAJOR.MINOR.PATCH, where PATCH is the repository's commit count —
# every commit is a patch release, so `v1.0.42` is the 42nd commit on the 1.0
# line. This is the same scheme CountRoster uses (scripts/version.mjs there);
# Tickers starts its life on the 1.0 line.
#
#   - MAJOR/MINOR are source constants, read out of
#     server/internal/version/version.go so there is exactly one declaration of
#     them in the tree. Bump them there.
#   - PATCH comes from `git rev-list --count HEAD`, which only exists at build
#     time: the Go binary gets it stamped in via -ldflags. scripts/build.sh and
#     the release workflow both call this file, so they can never disagree.
#
# Usage:
#   scripts/version.sh            # print e.g. v1.0.42
#   scripts/version.sh --patch    # print just the commit count (42)
#
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." >/dev/null 2>&1 && pwd)"
VERSION_GO="$ROOT/server/internal/version/version.go"

[ -f "$VERSION_GO" ] || { echo "version.sh: cannot find $VERSION_GO" >&2; exit 1; }

# Read `Major`/`Minor` out of the Go source that declares them. The pattern is
# anchored at both ends so it can only match the constant declaration itself,
# never a mention of the word in a comment.
read_const() {
  sed -nE "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*([0-9]+)[[:space:]]*$/\1/p" "$VERSION_GO" | head -n 1
}

MAJOR="$(read_const Major)"
MINOR="$(read_const Minor)"
[ -n "$MAJOR" ] || { echo "version.sh: could not find Major in $VERSION_GO" >&2; exit 1; }
[ -n "$MINOR" ] || { echo "version.sh: could not find Minor in $VERSION_GO" >&2; exit 1; }

# The commit count on HEAD, or 0 when it can't be known — no repo (a tarball,
# or a COPY that skipped .git), no git, or a *shallow* clone.
#
# Shallow is the trap, and it's why this isn't a bare `rev-list`: a clone made
# with `--depth 1` answers `rev-list --count HEAD` with `1`, which is not an
# error and not obviously wrong — it just quietly ships a build calling itself
# `1.0.1`. Refuse it. Patch 0 is the agreed "unstamped build" marker (it
# matches the Go default), and a version ending in `.0` is visibly a
# non-release rather than a plausible lie — the release workflow won't publish
# one.
#
# Anything building a release therefore needs the full commit graph:
# `fetch-depth: 0` on GitHub Actions, `--filter=blob:none` rather than
# `--depth 1` for a cheap clone that still carries all of it.
commit_count() {
  if ! git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
    printf '0'
    return
  fi
  if [ "$(git -C "$ROOT" rev-parse --is-shallow-repository 2>/dev/null || echo unknown)" = true ]; then
    echo "version.sh: shallow git clone — the commit count is not the real one," >&2
    echo "            reporting patch 0. Use --filter=blob:none, or fetch --unshallow." >&2
    printf '0'
    return
  fi
  # A failed probe (git older than 2.15) is not proof of shallowness — fall
  # through and let the count itself answer.
  git -C "$ROOT" rev-list --count HEAD 2>/dev/null || printf '0'
}

PATCH="$(commit_count)"

# Must stay byte-identical to version.String() in the Go package. `--patch`
# stays bare: that one feeds `-ldflags -X` as the value of version.Patch,
# which is the number alone.
if [ "${1:-}" = --patch ]; then
  printf '%s\n' "$PATCH"
else
  printf 'v%s.%s.%s\n' "$MAJOR" "$MINOR" "$PATCH"
fi
