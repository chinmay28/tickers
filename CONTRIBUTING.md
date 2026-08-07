# Contributing to Tickers

Contributions are welcome. This is a small project with a small surface, so the
bar is mostly "does it keep the deployment story true?"

## Before you start

```bash
git clone https://github.com/chinmay28/tickers.git && cd tickers
cd server && go vet ./... && go test -race ./...
```

You need Go >= 1.21 to bootstrap; the toolchain pinned in `server/go.mod` is
fetched automatically. There is nothing else to install — no Node, no package
manager, no front-end build.

Run it:

```bash
go run ./server/cmd/tickers serve --db /tmp/tickers.sqlite \
  --web-dist server/internal/web/assets     # edit the client and just reload
```

## The rules that aren't negotiable

These exist because people are running this on a Raspberry Pi and upgrading it
by re-running one command.

1. **Don't break the published payload.** `internal/publish/publish_test.go`
   pins the legacy format byte for byte. If your change makes those tests fail,
   the change is wrong — add a new format instead.
2. **Migrations are append-only and additive.** Never edit or reorder a shipped
   migration; add a new one. Never drop or rename a column an older binary
   reads — the quick start rolls back to the previous binary on a failed
   upgrade, and that only works if the new schema is a superset of the old.
   See [docs/design.md](./docs/design.md#migrations).
3. **No new runtime dependencies.** The deployable artifact is one static
   binary. A build-time Go module is fine if it earns its place; anything that
   has to exist on the target host is not.
4. **No front-end build step.** The client is hand-written HTML/CSS/ES modules
   embedded with `go:embed`. Adding a bundler would add Node to every Pi
   install.
5. **Keep `/api/health` honest.** It must fail when the database is
   unreachable; the upgrade rollback keys off it.

## Style

- `gofmt` clean — CI fails otherwise.
- Comments explain *why*, not *what*. The existing code is the reference for
  density and tone; match it.
- Validation belongs in `store`, behaviour in `engine`, and `api` stays a thin
  decode/status/encode layer.
- Tests assert behaviour with messages that say what broke, not `got != want`.

## Pull requests

- One concern per PR.
- Add tests for behaviour you add or fix. `go test -race ./...` must pass.
- Update the docs the change touches — README for user-facing behaviour,
  docs/design.md for structural decisions, DEPLOYMENT.md for anything an operator
  does.
- Add a line to `CHANGELOG.md` under **Unreleased**.
- Sign off your commits: `git commit -s`.

## Releasing

The version's patch number is the repository's commit count, so a release tag
is determined by the commit, not chosen:

```bash
scripts/version.sh          # e.g. v1.0.42 — this is the tag
git tag v1.0.42 && git push origin v1.0.42
```

The release workflow builds `linux/arm64` and `linux/amd64`, publishes them
with checksums, and uses the matching `## v1.0.42` section of `CHANGELOG.md` as
the release body. It refuses to publish if the tag doesn't match the version the
commit builds.

## License

Tickers is `AGPL-3.0-only`. By contributing you agree your contribution is
licensed under the same terms.
