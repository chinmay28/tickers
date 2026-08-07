#!/usr/bin/env bash
#
# Print the version this checkout builds as: vMAJOR.MINOR.PATCH, where the
# patch number is the repository's commit count.
#
#   scripts/version.sh            → v1.0.42
#   scripts/version.sh --patch    → 42
#
# Major and minor are read out of server/internal/version/version.go so there
# is exactly one place to bump them. The patch number can only come from git,
# which a compiled binary has no access to — so the build stamps it in at link
# time (see scripts/build.sh).
#
# A tree with no git history, or a shallow clone, reports patch 0. That is the
# marker for "unstamped development build", never a release: the release
# workflow refuses to publish it.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")/.." >/dev/null 2>&1 && pwd)"
VERSION_GO="$ROOT/server/internal/version/version.go"

[ -f "$VERSION_GO" ] || { echo "version.sh: cannot find $VERSION_GO" >&2; exit 1; }

read_const() {
  # Matches `Major = 1` / `Minor = 0` inside the const block.
  sed -nE "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*([0-9]+).*/\1/p" "$VERSION_GO" | head -n 1
}

MAJOR="$(read_const Major)"
MINOR="$(read_const Minor)"
[ -n "$MAJOR" ] && [ -n "$MINOR" ] || { echo "version.sh: could not read Major/Minor from $VERSION_GO" >&2; exit 1; }

PATCH=0
if git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  # A shallow clone counts only the commits it has, which would silently
  # produce a wrong (and much lower) version. Report 0 instead of a lie.
  if [ "$(git -C "$ROOT" rev-parse --is-shallow-repository 2>/dev/null || echo false)" = true ]; then
    PATCH=0
  else
    PATCH="$(git -C "$ROOT" rev-list --count HEAD 2>/dev/null || echo 0)"
  fi
fi

if [ "${1:-}" = --patch ]; then
  printf '%s\n' "$PATCH"
else
  printf 'v%s.%s.%s\n' "$MAJOR" "$MINOR" "$PATCH"
fi
