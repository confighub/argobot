# argobot

A ConfigHub bot that force-syncs Argo CD applications.

argobot connects to ConfigHub over the HTTP long-poll worker protocol, registers an `Argo` provider bridge, and force-syncs the corresponding Argo CD Application whenever a Unit bound to an Argo target is applied. The effect is that an apply feels like an immediate imperative deploy instead of waiting for Argo's next reconciliation.

This is the first "bot" built on ConfigHub's surviving server→client command/event delivery protocol. See the design in `confighub/docs/design/bots.md`.

## How it works

argobot is a worker with a single custom bridge:

- It advertises the `Argo` provider type with toolchain `Any` — it operates on the Target, not on Unit data, so it does not route by config format.
- On `Apply`, it reads the target's `BridgeHandle` as the Argo CD Application name (falling back to the Unit slug) plus the `AppNamespace`, `Prune`, and `Force` options, and calls the Argo CD REST API to sync. It reports `ApplyCompleted` or `ApplyFailed` back to ConfigHub, so the sync's synchronous success or failure surfaces on the Unit.
- Argo CD credentials are argobot's own authority, supplied by environment, never brokered through ConfigHub. Which Application to sync comes from the applied Unit/Target.

Force sync is on by default (this is a force-sync bot); set the `Force` option to `false` on a target to disable the apply force strategy.

Correctness belongs to Argo CD: it reconciles from its source regardless, so a missed or failed sync only loses immediacy, not correctness.

### Trigger model, and where this is going

The connector today exposes the bridge interface (`Apply`/`Refresh`/…) as its only trigger surface, so a force-sync is driven by applying a Unit on an Argo target. When ConfigHub grows an event-subscription channel, argobot will instead react to apply-completed events over this same long-poll transport — force-syncing automatically after an OCI apply, with no separate apply. The reporting direction (argobot pushing Argo/Kubernetes health back into ConfigHub) is also future work.

## Configuration

All configuration is via environment variables:

| Variable | Required | Description |
|---|---|---|
| `CONFIGHUB_URL` | yes | ConfigHub base URL, e.g. `https://hub.confighub.com` |
| `CONFIGHUB_WORKER_ID` | yes | Worker (bot) ID from ConfigHub |
| `CONFIGHUB_WORKER_SECRET` | yes | Worker secret from ConfigHub |
| `ARGOCD_SERVER` | yes | Argo CD API base URL, e.g. `https://argocd.example.com` |
| `ARGOCD_AUTH_TOKEN` | yes | Argo CD API bearer token |
| `ARGOCD_INSECURE` | no | `true` to skip TLS verification (self-signed Argo CD) |

## Setup

Build the CLI (`bin/cub`) from the ConfigHub repo, then:

```sh
# 1. Create the worker (bot identity) in the space that owns the Argo targets.
cub worker create argobot --space my-space

# 2. Read its ID and secret for argobot's environment.
cub worker get argobot --space my-space

# 3. Create an Argo target bound to that worker. The BridgeHandle is the Argo CD
#    Application name; options carry namespace / prune / force.
cub target create my-app-argo \
  --space my-space \
  --worker argobot \
  --provider Argo \
  --bridge-handle my-app \
  --option AppNamespace=argocd \
  --option Force=true

# 4. Bind a Unit to the target and apply it to trigger a force-sync.
cub unit set-target my-app-argo --unit my-app --space my-space
cub unit apply my-app --space my-space
```

(Exact `cub` flags may vary by CLI version; `cub target create --help`.)

## Deploy

argobot is meant to run in the cluster alongside Argo CD, with its own manifests managed in ConfigHub. `deploy/argobot.yaml` is a ready-to-store Unit: a ConfigMap for non-secret settings and a Deployment. Create the `argobot-secrets` Secret (`CONFIGHUB_WORKER_ID`, `CONFIGHUB_WORKER_SECRET`, `ARGOCD_AUTH_TOKEN`) out of band or via your secret store.

## Build

```sh
go build ./...        # compile
docker build -t ghcr.io/confighub/argobot:dev .
```

CI (`.github/workflows/build.yml`) builds and pushes the image to `ghcr.io/confighub/argobot` on pushes to `main` and version tags.
