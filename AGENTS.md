# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

`AGENTS.md` is the real file; `CLAUDE.md` is a symlink to it so every agent reads the same text.
Workspace-wide conventions (git signing, branch naming, PR rules, CI patterns) live in `~/repos/htb/CLAUDE.md`
and are not repeated here.

## What this repo is

Hack The Box's maintained fork of [hipages/php-fpm_exporter](https://github.com/hipages/php-fpm_exporter):
a Prometheus exporter that talks FastCGI directly to PHP-FPM's `/status` page. No webserver in the path.

- `origin` is `hackthebox/php-fpm_exporter`, `upstream` is `hipages/php-fpm_exporter`. Both remotes are configured.
- Apache-2.0. Upstream file headers stay as-is; HTB-authored changes to an upstream file get an added
  `Modified <year> by Hack The Box: <what and why>` note (see the top of `phpfpm/phpfpm.go`). New HTB files
  carry an HTB copyright plus `SPDX-License-Identifier: Apache-2.0` (see `phpfpm/fcgi/fcgi.go`).
- The module path is `github.com/hackthebox/php-fpm_exporter`. Renaming it breaks every import and the
  release tooling; don't.
- There is no `CONTRIBUTING.md`. `README.md` is user-facing docs and the contract for flags, env vars,
  and the published metric list.

## Commands

Go toolchain is pinned by `mise.toml`; `mise install` before anything else. Three places name the Go
version and they are kept identical: `mise.toml` (local), the `go` directive in `go.mod` (what CI resolves,
via `go-version-file: go.mod`, and therefore what the released binary is built with), and the builder stage
in `Dockerfile`. Moving one alone either has no effect on CI or leaves the ECR image on an older stdlib,
which is what Grype reports.

```bash
make test          # go test -short ./...  (no test reads testing.Short, so this is the full unit suite)
make lint          # golangci-lint run  (config in .golangci.yml, v2 schema)
make fmt           # goimports -w .
make test-coverage # writes .cover/cover.out
make test-e2e      # bats test/e2e.bats -- needs docker-compose + bats-core, see README "Development"

go test ./phpfpm -run TestCountProcessState -v   # single test
go run . server --log.level=debug                # run locally against 127.0.0.1:9000
```

## Architecture

`main.go` injects version/commit/date (goreleaser ldflags) and calls `cmd.Execute()`.

**`cmd/` is wiring only.** Anything with logic lives under `internal/`, where it can be tested; a command
body that grows past building a config and mapping an error to an exit code belongs there instead.

- `cmd/` — cobra CLI. `root.go` owns logging, viper config, and `mapEnvVars`, which is the only place
  env-var-to-flag mapping happens (viper's `BindEnv` is deliberately not used; see the comment in
  `cmd/server.go`). Adding a flag means adding it to the `envs` map too, and to the options table in
  `README.md`. Three commands: `get` (one-shot dump), `server` (Prometheus endpoint), `version`.
- `internal/server/` — the `server` command's body. Takes its registry, logger and listener as
  dependencies, so the endpoint, both discovery modes and the shutdown path are testable.
- `internal/get/` — the `get` command's body. `Run(cfg, w io.Writer) error` writes the chosen format to an
  injected writer, so each format can be asserted. A scrape failure is returned but does not suppress the
  output; `cmd/get.go` turns it into a non-zero exit code.
- `phpfpm/` — the library. `PoolManager` holds a slice of `Pool` (one per scrape URI); `Exporter`
  implements `prometheus.Collector` and calls `PoolManager.Update()` on every scrape, fanning out one
  goroutine per pool.
- `phpfpm/fcgi/` — HTB's own minimal FastCGI client, replacing `tomasen/fcgi_client`. It exists solely
  because every request must run under a hard connection deadline: without one, a stalled PHP-FPM
  `/status` (e.g. during a graceful reload) leaked a goroutine and a socket per scrape. Keep the deadline.

### Two target-discovery modes

`server` runs in exactly one of them:

- **Static** — `--phpfpm.scrape-uri` list, added to the `PoolManager` once at startup, pod label empty.
- **Kubernetes auto-tracking** — `--k8s.autotracking`. `phpfpm/pod_discovery.go` lists pods matching
  `--k8s.pod-labels` in `--k8s.namespace`, then runs a `RetryWatcher` in a background goroutine. The
  initial list is what supplies the `ResourceVersion` the retry watcher needs. Pools are added when a pod
  reaches `Running` with an IP and removed on delete; `PoolManager.PodPhases` tracks the last seen phase
  so a Pending→Running transition is only acted on once. Requires in-cluster config (`rest.InClusterConfig`),
  so it cannot run outside a pod.

### Concurrency invariant

The watcher goroutine mutates a `PoolManager` and publishes it via `Exporter.UpdatePoolManager`, which
takes the exporter mutex and copies the struct. `Exporter.Collect` holds the same mutex. That mutex is the
only thing serialising discovery against scraping — anything new that touches `Exporter.PoolManager` has
to go through `UpdatePoolManager`, not reach in directly.

### Metric label contract

`phpfpm/exporter.go` defines two families of label set, `*WithPod` and `*NoPod`, and an
`Exporter.*LabelValues` method per set. Label names and order are a public contract that downstream
alerts key off. The dashboard in `grafana/` selects on metric names only, not on these labels, so it
is not the thing that constrains them.

`phpfpm_pod` is an HTB addition and is emitted only under `--k8s.autotracking`, selected with
`phpfpm.WithPodLabel()` at `NewExporter`. Static pools have no pod name, so the label is absent rather
than empty; Prometheus treats an empty label value as equivalent to an absent one, so emitting it was
pure noise on VM deployments. **The choice is per exporter, never per pool**: client_golang refuses two
descriptors that share a metric name but disagree on label names, so a mixed process cannot exist. That
is safe because `server` runs in exactly one discovery mode.

The label cannot be replaced by Prometheus relabeling in HTB's topology: one exporter per namespace
fans out to many pods, so the scrape target is the exporter and Prometheus never sees the pod.

## CI and release

Workflows in `.github/workflows/`, all actions pinned by SHA:

- `test.yml` — lint (`golangci-lint`, `only-new-issues: true`, so pre-existing
  findings do not block), `go test ./...`, and a goreleaser `--snapshot` build scanned with Anchore. It
  handles `pull_request` and `push` in one file deliberately: code scanning keys an analysis on the
  workflow file path plus job name, so scanning the two events from two files produced configurations that
  could never match, and every PR reported "1 configuration not found" instead of a vulnerability diff.
- `release.yml` — manual or monthly. semantic-release tags from `master`, then goreleaser publishes
  binaries plus multi-arch images to GHCR only (`.goreleaser.yml`, `Dockerfile.goreleaser`).
- `build-and-push.yml` — on a `v*` tag, builds the plain `Dockerfile` and pushes to HTB's ECR via OIDC.
  This is the image the HTB clusters actually run, and it is a separate build from the goreleaser one.

`Dockerfile` (ECR path) and `Dockerfile.goreleaser` (public path) are both live. A build change usually
needs both. Keep every base image a literal `FROM image:tag`: Dependabot's docker ecosystem parses those
and cannot resolve a tag built from an `ARG`, so an indirected pin is one it will silently never update.

## Gotchas

- `test/e2e.bats` asserts on metric lines that predate the `phpfpm_pod` label and nothing in CI runs bats,
  so the e2e suite is not a gate and its assertions may not match current output. Check before trusting it.
- `README.md` badges still point at the upstream hipages repo, so they are not a reliable source for "where
  does this live". `.releaserc` used to as well, which silently disabled releases: semantic-release compared
  this `master` against upstream's, found it behind and published nothing.
- **Releases have never been cut by the `Release` workflow.** Every tag so far was pushed by hand. One thing
  still blocks the automated path: a tag pushed by semantic-release under the default `GITHUB_TOKEN` does
  not trigger `build-and-push.yml`, so the ECR image the clusters run would never be built. A tag pushed by
  a human does trigger it, which is why that is the working path.
- **This fork publishes images to GHCR only, never Docker Hub.** Pushing `php-fpm_exporter` to Docker Hub
  belongs to upstream hipages. The release job therefore has no Docker Hub login, and `.goreleaser.yml`
  carries no bare `hackthebox/php-fpm_exporter` image or manifest. The Anchore scan in `test.yml` picks the
  built image with a `--filter=reference=` on the *ghcr.io* name; that filter
  matches the repository name exactly, so it silently finds nothing if the image templates are renamed.
- The repo has no `.crap-gated` marker, so the touchstone per-function gate does not apply here.
- `CountProcessState` mirrors PHP-FPM: a child is idle only while accepting, every other stage is active, and
  an unrecognised stage counts as active so it cannot vanish from the total. The stage list comes from
  `sapi/fpm/fpm/fpm_request.c` in php-src; `Creating` was missing for years and produced the log spam in
  hipages/php-fpm_exporter#419, while dropping Finishing/Ending/Info from the total is
  hipages/php-fpm_exporter#322.
- `log` in `phpfpm` defaults to a discarding logger and `SetLogger` ignores nil, because it is a package
  global that a library caller need never set.
- `PoolManager.Update` returns the joined per-pool scrape errors. `Pool.error` already logs each one, so
  callers should not log the aggregate again; `cmd/get.go` turns it into a non-zero exit code.
