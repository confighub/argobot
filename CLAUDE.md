# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

argobot is a ConfigHub bot (Go, single binary, `package main` at the repo root). It claims a ConfigHub Channel as a service account, and on each `release.published` event forces the GitOps controller to sync immediately. It also watches the controller's resources and writes each Release's `LiveStatus` back to ConfigHub, and (when `CONFIGHUB_TARGET` is set) stores cluster facts on the Target at startup. Three controllers are supported, chosen by `GITOPS_CONTROLLER`: `argocd` (default), `sveltos`, `flux`. README.md is the authoritative behavioral spec — keep it in sync with code changes.

## Commands

```sh
go build ./...
go vet ./...
go test ./...
go test -run TestResolveAppNames .          # single test in the root package
go test -run TestRepoURLs ./flux            # single test in a backend package
go run . --print-subscriptions               # Channel subscriptions JSON
go run . docgen env                          # env vars as JSON Schema
docker build -t ghcr.io/confighub/argobot:dev .
```

Scripts (run from repo root):
- `scripts/test-package.sh` — renders/validates the installer package in `manifests/package` (needs `installer` ≥0.3.0, `kustomize`, `yq`, `go`). CI runs it.
- `scripts/set-version.sh vX.Y.Z` — bumps the image tag in all three manifests and the package; `--check` verifies. Release = run this, merge, then push the `vX.Y.Z` tag (`release.yml` fails otherwise).
- `scripts/gen-third-party-licenses.sh` — regenerate and commit `THIRD_PARTY_LICENSES.txt` after any dependency change; CI fails if stale or if a copyleft dep is linked.
- `scripts/verify-no-personal-names.sh` — CI check. Prose says "the team"/"a colleague" or an issue number, never a person's name; use `$HOME` and `user@example.com` in examples.

## Architecture

`main.go` runs three things under one errgroup sharing one `cubapi.Client` (authenticated by signing an assertion with the private JWK):
1. **Channel listener** (`events.go`) → `makeEventHandler` resolves an event's Space slug to controller object names and calls `Syncer.Sync` on each. Resolution: `ARGO_APP` if set (Argo only), otherwise every object whose OCI source URL ends in `/space/<slug>` (from `Syncer.RepoURLs`) plus the object named `<slug>`.
2. **Live-status reporter** (`reporter*.go`) — best-effort; failure to build it only warns.
3. **Cluster facts** (`facts.go`) — once at startup, best-effort.

Two seams carry the controller switch:
- **`Syncer` interface** (`syncer.go`), implemented in subpackages `kube/` (Argo CD: patch `argocd.argoproj.io/refresh`), `sveltos/` (patch `profile.projectsveltos.io/redeploy` with a timestamp), `flux/` (patch `reconcile.fluxcd.io/requestedAt` with a timestamp). All go through the Kubernetes dynamic client; `kube.NewDynamicClient`/`kube.RestConfig` are shared (in-cluster or kubeconfig).
- **`statusReporter` interface** (`reporter.go`, `newReporter`). Each is an informer + workqueue with a single worker goroutine (so its caches need no locks), embedding the shared `liveStatusWriter` (merge-patches `LiveStatus` onto a Release).
  - Argo (`reporter.go`) and Flux (`reporter_flux.go`) map controller state to a Release by **ManifestDigest** via the shared `releaseLookup`.
  - Sveltos (`reporter_sveltos.go`) can't — Sveltos records no digest — so it reads `HealthCheckReport`s, decodes each collected object's `confighub.com/origin` annotation (Space/Unit/Revision), and finds the newest published Release whose Tag covers every observed Unit revision.
  - Status normalization from controller vocab to ConfigHub's lives in `projectStatus` (Argo), `livestatus_sveltos.go`, and `livestatus_flux.go`.
  - Writes are deduplicated (Argo/Sveltos against the last projection in memory; Flux against the status already in ConfigHub) so idle objects and restarts produce no writes.

## Configuration coupling

Env vars are declared once in `envVars` (`config.go`) with `env`/`json`/`default`/`enum`/`description` tags; `loadConfig` applies defaults itself (an empty var equals unset). Adding or changing a variable requires updating, together:
- `manifests/package/validation/env.schema.json` — regenerate with `go run . docgen env > manifests/package/validation/env.schema.json` (`test-package.sh` diffs it).
- The `env` list in the relevant `manifests/{argobot,flux/argobot,sveltos/argobot}.yaml` (each lists **every** supported variable) and the package's `bases/default/argobot.env` or `components/<controller>/*.env`.
- The README configuration table.

Removed variables go in `retiredEnv` (warned about, not silently ignored).

## Manifests

`manifests/argobot.yaml` (Argo CD), `manifests/flux/argobot.yaml`, `manifests/sveltos/argobot.yaml` are copied into the image at `/manifests/{argocd,flux,sveltos}.yaml` (last layer, read by `cub cluster attach`). They pin a release tag (never `latest`) and leave `CONFIGHUB_CHANNEL`/`CONFIGHUB_TARGET` as `confighubplaceholder`. The `argobot-secrets` Secret is deliberately never part of any manifest. The read-only ClusterRole intentionally excludes Secrets (core group is enumerated without `secrets`). `manifests/package` is the installer package (base + one component per controller).
