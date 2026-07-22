# argobot

A ConfigHub bot that syncs Argo CD applications.

argobot connects to ConfigHub over the HTTP long-poll protocol and subscribes to ConfigHub's event log. When ConfigHub records that a deploy happened — a Unit apply completed, or a Release was published — argobot triggers a sync of the corresponding Argo CD Application, so the change takes effect immediately instead of waiting for Argo's next reconciliation.

This is the first "bot" implementation that takes a different approach than the original Bridge design. We expect to replace all bridge functionality with this kind of bot mechanism.

## How it works

argobot connects as a ConfigHub worker and reacts to events over the long-poll connection:

- It subscribes to the `apply.completed` and `release.published` event types. By default it scopes delivery to the Targets its own worker is the bridge for — on startup it asks ConfigHub which Targets name its worker and subscribes to each — so a bot reacts only to its own deploys without being told the Target IDs. `CONFIGHUB_EVENT_TARGET_ID` overrides this with a single explicit Target, and `CONFIGHUB_EVENT_SPACE_ID` further narrows delivery to one Space. The subscription is declared when argobot connects.
- On a delivered event it resolves which Argo CD Application to sync (see below) and triggers a sync. The reaction is argobot's own; the event only says the desired state for a (Space, Target) changed.
- The delivery cursor is held by ConfigHub, keyed by the worker and the subscription name, so a restart resumes where it left off without argobot keeping any local state.

### Sync modes

`ARGO_SYNC_MODE` selects how argobot triggers the sync:

- **`kubernetes`** (default) — argobot patches the Application's `argocd.argoproj.io/refresh` annotation through the Kubernetes API. Argo re-compares the Application against its source (a **hard** refresh re-resolves the source, including an OCI tag→digest, so Argo picks up a freshly published bundle rather than replaying a cached digest) and then, **if the Application has auto-sync enabled, deploys**. If auto-sync is off, argobot's refresh does not deploy — auto-sync is the deploy gate. Credentials are the pod's ServiceAccount; there is no Argo CD API token to set up.
- **`argocd`** — argobot calls the Argo CD REST `/api/v1/applications/{name}/sync` endpoint. This deploys **regardless** of the Application's auto-sync setting. It needs an Argo CD API base URL and bearer token, which are argobot's own authority, supplied by environment and never brokered through ConfigHub.

Resolving the Application. When `ARGO_APP` is set, it is the single Application every matching event syncs — the simplest deployment. When it is not set, a `release.published` event carries its Space slug, which by the app-name == space-slug convention names the Application; an `apply.completed` event without `ARGO_APP` cannot be resolved and is skipped with a warning.

Correctness belongs to Argo CD: it reconciles from its source regardless, so a missed or failed sync only loses immediacy, not correctness.

The reporting direction — argobot pushing Argo / Kubernetes health back into ConfigHub — is the next step and not yet built. It is also why the default `kubernetes` sync mode already uses the Kubernetes API: argobot will need that client to watch resource health anyway.

## Configuration

All configuration is via environment variables:

| Variable | Required | Description |
|---|---|---|
| `CONFIGHUB_URL` | yes | ConfigHub base URL, e.g. `https://hub.confighub.com` |
| `CONFIGHUB_WORKER_ID` | yes | Worker (bot) ID from ConfigHub |
| `CONFIGHUB_WORKER_SECRET` | yes | Worker secret from ConfigHub |
| `ARGO_SYNC_MODE` | no | `kubernetes` (default) or `argocd`. See [Sync modes](#sync-modes). |
| `ARGO_NAMESPACE` | no | _kubernetes mode._ Namespace holding the Argo CD Application resources. Defaults to `argocd`. |
| `ARGO_REFRESH_TYPE` | no | _kubernetes mode._ `hard` (default) or `normal`. |
| `ARGOCD_SERVER` | argocd mode | Argo CD API base URL, e.g. `https://argocd.example.com` |
| `ARGOCD_AUTH_TOKEN` | argocd mode | Argo CD API bearer token |
| `ARGOCD_INSECURE` | no | _argocd mode._ `true` to skip TLS verification (self-signed Argo CD) |
| `CONFIGHUB_SUBSCRIPTION_NAME` | no | Subscription name; keys the server-held delivery cursor, so it must be stable across restarts. Defaults to `argobot`. |
| `CONFIGHUB_EVENT_SPACE_ID` | no | Scope delivery to one Space (UUID). Empty means every Space. |
| `CONFIGHUB_EVENT_TARGET_ID` | no | Override the Target scope with one Target (UUID). Empty (default) auto-scopes to the Targets the worker is the bridge for. |
| `ARGO_APP` | no | The single Argo CD Application every matching event syncs. If unset, the Application is resolved from the event (a release carries its Space slug). |
| `ARGO_APP_NAMESPACE` | no | _argocd mode._ Argo CD Application namespace passed to the REST sync request (apps-in-any-namespace). |
| `ARGO_PRUNE` | no | _argocd mode._ `true` to prune on sync. |
| `ARGO_FORCE` | no | _argocd mode._ `true` to use Argo's force strategy on sync. |

When `ARGO_SYNC_MODE` is unset, argobot runs in `kubernetes` mode and needs no Argo CD credentials; it uses the in-cluster ServiceAccount (or a local kubeconfig when run out of cluster). Set `ARGO_SYNC_MODE=argocd` to use the REST API instead, which then requires `ARGOCD_SERVER` and `ARGOCD_AUTH_TOKEN`.

## Setup

Install the `cub` CLI (see [docs.confighub.com](https://docs.confighub.com)), then:

```sh
# 1. Create the worker (bot identity) in the space that owns the Argo targets.
cub worker create argobot --space my-space

# 2. Read its ID and secret for argobot's environment.
cub worker get argobot --space my-space
```

Then run argobot with the worker credentials, pointing it at the Application to sync. In the default `kubernetes` mode it uses the ambient Kubernetes credentials (in-cluster ServiceAccount, or your local kubeconfig) — no Argo CD token:

```sh
CONFIGHUB_URL=https://hub.confighub.com \
CONFIGHUB_WORKER_ID=... CONFIGHUB_WORKER_SECRET=... \
ARGO_NAMESPACE=argocd \
ARGO_APP=my-app \
argobot
```

To use the Argo CD REST API instead (deploys regardless of auto-sync), set `ARGO_SYNC_MODE=argocd` and supply its credentials:

```sh
CONFIGHUB_URL=https://hub.confighub.com \
CONFIGHUB_WORKER_ID=... CONFIGHUB_WORKER_SECRET=... \
ARGO_SYNC_MODE=argocd \
ARGOCD_SERVER=https://argocd.example.com ARGOCD_AUTH_TOKEN=... \
ARGO_APP=my-app \
argobot
```

A `cub unit apply` or a `cub release` for the scoped (Space, Target) now emits an event that argobot receives and syncs on. By default argobot reacts only to deploys on the Targets its worker is the bridge for (discovered at startup). Set `CONFIGHUB_EVENT_TARGET_ID` to override that with a single Target, or `CONFIGHUB_EVENT_SPACE_ID` to further narrow to one Space.

(Exact `cub` flags may vary by CLI version; `cub --help`.)

## Deploy

argobot runs in the cluster alongside Argo CD. `manifests/argobot.yaml` is a complete, self-documenting deployment: a Namespace, a ServiceAccount, its RBAC (see below), and a Deployment whose `env` list carries **every** supported variable — defaults filled in, and the ones that must be absent by default commented out. The Deployment reads its credentials from a Secret named `argobot-secrets` via `secretKeyRef`; that Secret is intentionally **not** part of the manifest — supply it out of band (secret store / External Secrets, or `kubectl create secret generic argobot-secrets --from-literal=...`) so the bundle never ships or overwrites credential material. Expected keys: `CONFIGHUB_WORKER_ID`, `CONFIGHUB_WORKER_SECRET`, and (argocd mode only) `ARGOCD_AUTH_TOKEN`.

RBAC comes in two parts. A **`Role`/`RoleBinding` in the Argo CD namespace** (`argocd` by default) grants read+write there — argobot patches an Application's refresh annotation to force a sync, and may manage other Argo CD resources; being namespaced, it grants nothing outside `argocd`. A **`ClusterRole`/`ClusterRoleBinding`** grants cluster-wide **read-only** (`get`/`list`/`watch`) for the upcoming health/status reporting across all namespaces — deliberately **excluding Secrets** (the core API group is enumerated without `secrets`; other groups are wildcarded, and Secrets exist only in core). Add a CRD's API group to that second rule to extend coverage. The Namespace, ServiceAccount, and Deployment are argobot's own — adjust the namespaces to match your cluster.

### Config bundle

`.github/workflows/release.yml` packages `manifests/` into an OCI bundle and pushes it to `ghcr.io/confighub/configs/argobot:latest`. The bundle **floats** — a single moving `:latest` — but each cut pins a concrete released image: the committed manifest carries `argobot:latest`, and the workflow substitutes it with the current released version (resolved from git tags) before publishing, failing the build if the pin does not take. Load it into ConfigHub as a component base:

```sh
cub variant upload --component argobot --variant base --granularity per-file \
  oci://ghcr.io/confighub/configs/argobot
```

## Build and release

```sh
go build ./...        # compile
docker build -t ghcr.io/confighub/argobot:dev .
```

Two workflows, two cadences:

- `.github/workflows/build.yml` builds and pushes **development** images to `ghcr.io/confighub/argobot` on pushes to `main` and on PRs (tags `main`, `pr-N`, `sha-…`). No semver, no `:latest`.
- `.github/workflows/release.yml` cuts a **release** when you push a `vX.Y.Z` git tag: it builds the semver image (`:vX.Y.Z`, plus `:latest`) and republishes the config bundle pinned to it. The bundle is also republished on every `main` push, pinned to the current latest tag, so a config-only change ships without a new image. The pinned version is never committed, so there is no commit-to-release cycle.
