// Package version carries the application version.
//
// The scheme is calendar-based: vYEAR.MONTH.PATCH, where the patch number is
// the repository's commit count — every commit is a patch release, so
// `v2026.8.42` is the 42nd commit on the 2026.8 line. The month is written as
// a plain number, not zero-padded: that keeps the string valid semver, which
// forbids a leading zero, and nothing here orders versions by sorting text.
//
// Year and Month are declared here in source and bumped by hand when a release
// line opens — deliberately not read from the build clock, which would move the
// version without a commit and make a rebuild of an old tree disagree with what
// it originally shipped. The patch number can only come from git, which a
// compiled binary has no access to, so it is stamped at link time instead:
//
//	go build -ldflags "-X github.com/chinmay28/tickers/server/internal/version.Patch=$(git rev-list --count HEAD)"
//
// scripts/build.sh does this for you, and scripts/version.sh is the one place
// that knows how to compute it — it reads the two constants below with an
// anchored regex, so keep them one-per-line in `Name = digits` form.
package version

import "strconv"

// Year and Month of the release line, bumped by hand. Month is a calendar
// month, 1–12.
//
// There is no semantic major/minor here: the leading numbers say *when* a
// release line opened, not what it promises about compatibility. The one
// compatibility promise this project makes — the published payload's format —
// is pinned by internal/publish's tests rather than by a version number, and
// anything that would break it is called out in CHANGELOG.md.
const (
	Year  = 2026
	Month = 8
)

// Patch is the repository's commit count, stamped at link time (see the
// package comment). A bare `go build` leaves it at "0": patch 0 means an
// unstamped development build, never a release.
var Patch = "0"

// String renders the full version, `v`-prefixed to match how the project tags
// releases (v2026.8.0). This is the one rendering — it's what the CLI prints,
// what /api/health reports, and what the web client shows in its header.
func String() string {
	return "v" + strconv.Itoa(Year) + "." + strconv.Itoa(Month) + "." + Patch
}
