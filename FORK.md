# Pulumi fork of golang-migrate

This is Pulumi's fork of [`golang-migrate/migrate`](https://github.com/golang-migrate/migrate). It exists to carry a small patchset on top of upstream, primarily a MySQL metadata-lock retry behavior that upstream has not adopted, and to ship a mysql-only build with a reduced dependency surface.

If you're a consumer (pulumi/service, pulumi-self-hosted-installers), skip to [Consumer integration](#consumer-integration). If you're maintaining the fork, read on.

## Why this fork exists

1. **MySQL metadata-lock retries.** The upstream MySQL driver fails immediately when a migration's `ALTER TABLE` or `DROP` hits a metadata lock held by a long-running query. Our patch adds a configurable retry loop so deployments don't fail on transient lock contention. See `database/mysql/mysql.go`.
2. **Reduced dependency surface.** Upstream supports ~15 database drivers and ~10 sources. We only use MySQL + the local file source. The fork is pruned to those, dropping ~100 transitive dependencies and the security alerts that come with them.
3. **Build shape adapted for our consumers.** A simplified `make build-cli` target with a single overridable output path, suited to the Docker builds in `pulumi/service` and the install scripts in `pulumi-self-hosted-installers`.

## What we carry

Five commits on top of `upstream/master`:

| Commit subject | What it changes |
|---|---|
| Add metadata lock retries to the mysql driver | The actual feature patch. ~60 lines in `database/mysql/mysql.go`. Adds retry-on-`ER_LOCK_WAIT_TIMEOUT` behavior with configurable count and backoff. Default retry count is 0 (opt-in). |
| Restore make target behavior to what our existing scripts expect | Reshapes `build-cli` to a single output path via `$(CLI_BUILD_OUTPUT)`. Used by pulumi/service and pulumi-self-hosted-installers build scripts. |
| Rename module path to github.com/pulumi/golang-migrate/v4 | Mechanical sed pass renaming the module. Re-run on every upstream sync (see below). |
| Prune to mysql-only: drop non-mysql drivers and sources | Deletes all non-MySQL database drivers, non-`file`/`iofs`/`stub`/`testing` sources, the `internal/cli/build_*.go` build-tag registration files, and the deprecated `cli/` package. Hardcodes mysql + file imports directly in `cmd/migrate/main.go`. |
| Slim Makefile, drop dead Dockerfiles/Travis, tidy go.mod | Removes the `DATABASE`/`SOURCE` Makefile variables, the broken Dockerfile variants, `.travis.yml`, and `docker-deploy.sh`. `go mod tidy` drops the transitive deps from removed drivers. |
| Bump Go toolchain to 1.26.3 and docker/docker to v28.5.2 | Vulnerability hygiene. The `toolchain` directive keeps `go 1.24.0` as the minimum (so consumers on older Go aren't forced to upgrade) but builds us with 1.26.3+ to clear stdlib CVEs. |

The substantive hand-maintained edits are tiny: ~73 lines in `mysql.go`, a slim Makefile, and a 6-line hardcoded-imports patch to `cmd/migrate/main.go`. Everything else is mechanical (sed, deletions, `go mod tidy` output).

## Maintaining the fork

### Routine upstream sync

```bash
git fetch upstream
git checkout -b kmosher/upstream-sync-$(date +%Y%m%d) upstream/master

# 1. Cherry-pick the three substantive commits
git cherry-pick <sha of "Add metadata lock retries...">
git cherry-pick <sha of "Restore make target behavior...">
git cherry-pick <sha of "Prune to mysql-only...">

# 2. Regenerate the module-path rename (mechanical, no conflicts)
git grep -l 'github.com/golang-migrate/migrate/v4' \
  | xargs sed -i '' 's|github.com/golang-migrate/migrate/v4|github.com/pulumi/golang-migrate/v4|g'
git add -A && git commit -m "Rename module path to github.com/pulumi/golang-migrate/v4"

# 3. Cherry-pick the Makefile/Dockerfile cleanup (may need regen of go.sum portion)
git cherry-pick <sha of "Slim Makefile...">
go mod tidy
git add go.mod go.sum && git commit --amend --no-edit

# 4. Cherry-pick the toolchain/docker bump
git cherry-pick <sha of "Bump Go toolchain...">

# 5. Verify
go build ./... && go vet ./... && go test -short ./...

# 6. Open PR
git push -u origin kmosher/upstream-sync-YYYYMMDD
gh pr create --draft --base master --title "Sync to upstream <version>"
```

Find the previous SHAs with: `git log master --no-merges --grep='Add metadata lock retries'` etc.

### Conflict expectations

| Commit | Probability | How to resolve |
|---|---|---|
| Lock retries (mysql.go) | Medium | Only file with semantic edits. If upstream rewrites `Lock`/`Unlock` or the connection init, you'll get a 3-way merge in one file. Read both sides, preserve the retry loop. |
| Makefile build-cli | Low | Upstream rarely touches the `build-cli` shape |
| Module rename (sed) | None | Sed regenerates fresh each time |
| Prune deletions | Low-medium | "Upstream modified a file we deleted" — `git rm <file>` and continue |
| `go mod tidy` | None | Deterministic |
| Toolchain/docker bump | None | Trivial |

### Enable `git rerere` once

```bash
git config rerere.enabled true
```

Git will remember conflict resolutions across rebases. The mysql.go conflict you resolve once gets auto-applied next time if it recurs in the same shape.

### Adding a new pulumi-only patch

Branch off `master`, make your change, commit, open a PR. The patchset grows by one commit. On the next upstream sync, cherry-pick it along with the others. Keep commits focused — one logical change per commit, so they're individually cherry-pickable.

## Versioning

Tags follow the scheme `vUPSTREAM-pulumi.N`:

- `UPSTREAM` is the upstream release tag we've last synced past (e.g. `v4.19.1`).
- `N` is the pulumi patch number on that upstream base. Bump on every release.

| Scenario | New tag |
|---|---|
| First pulumi release on upstream `v4.19.1` | `v4.19.1-pulumi.1` |
| Pulumi-only patch on the same upstream base | `v4.19.1-pulumi.2` |
| Sync to upstream `v4.20.0`, no pulumi changes | `v4.20.0-pulumi.1` |
| Sync to upstream `v4.20.0` with pulumi changes in the same PR | `v4.20.0-pulumi.1` |
| Pulumi-only patch after that sync | `v4.20.0-pulumi.2` |

### Tagging a release

```bash
git checkout master && git pull
git tag -a v4.X.Y-pulumi.N -m "Brief release notes"
git push origin v4.X.Y-pulumi.N
```

### Critical: never push upstream's tags to `origin`

When you fetch from the `upstream` remote, you get upstream's tags locally (`v4.19.1`, `v4.20.0`, etc.). **Don't push them to `origin`.** If `v4.20.0` lands on `origin` pointing at upstream's tip (without our patches), consumers running `go get @latest` could resolve to a tag that lacks our patches.

In Go semver, `v4.X.Y-pulumi.N` sorts as a pre-release of `v4.X.Y`, meaning a non-prerelease `v4.X.Y` tag on the same repo would sort higher. As long as `origin` has only pulumi-tagged versions, consumers always resolve to ours.

## Consumer integration

### Go module consumers

Pin to a tag in `go.mod`:

```
require github.com/pulumi/golang-migrate/v4 v4.X.Y-pulumi.N
```

Hardcoded imports mean consumers don't need build tags. The minimum useful import set is:

```go
import (
    "github.com/pulumi/golang-migrate/v4"
    _ "github.com/pulumi/golang-migrate/v4/database/mysql"
    _ "github.com/pulumi/golang-migrate/v4/source/file"
)
```

### CLI binary consumers

`make build-cli` produces a statically-linked `migratecli` binary at `$(CLI_BUILD_OUTPUT)` (default `/go/bin/migratecli`). The MySQL driver and file source are baked in — no `DATABASE` or `SOURCE` make variables needed.

```bash
git clone --depth 1 --branch v4.X.Y-pulumi.N \
  https://github.com/pulumi/golang-migrate.git
cd golang-migrate
CLI_BUILD_OUTPUT=/path/to/migratecli make build-cli
```

### Bazel consumers (pulumi/service)

Use `archive_override` pointed at the tagged tarball. Hardcoded imports mean no source patches are needed — the legacy `golang-migrate-mysql-driver.patch` and `golang-migrate-mysql-build.patch` should be removed from `third_party/patches/`.

```python
archive_override(
    module_name = "com_github_pulumi_golang_migrate_v4",
    strip_prefix = "golang-migrate-<sha-of-tag>",
    urls = ["https://github.com/pulumi/golang-migrate/archive/refs/tags/v4.X.Y-pulumi.N.tar.gz"],
)
```

## Vulnerability scanning

Run `govulncheck ./...` to scan the tree. After a sync, expect:

- **CLI binary + library import surface**: clean. The production import graph is `go-sql-driver/mysql` plus our own packages — no third-party CVEs.
- **Test-only paths (`testing/docker.go`, `dktesting/`)**: may report `docker/docker` advisories. These are unreachable from consumer code. Leave them be unless upstream `docker/docker` ships a fix worth pulling in.
- **Stdlib**: cleared by the `toolchain` directive. Bump the toolchain version when a new Go patch release ships with CVE fixes.

GitHub Dependabot will rescan `go.sum` after each merge and converge on the same answer (typically within a few minutes). Dependabot may surface alerts on indirect deps even when govulncheck reports them as unreachable — trust govulncheck's reachability analysis.
