# argobot

A ConfigHub bot that force-syncs Argo CD applications.

argobot connects to ConfigHub over the HTTP long-poll protocol and subscribes to ConfigHub's event log. When ConfigHub records that a deploy happened — a Unit apply completed, or a Release was published — argobot force-syncs the corresponding Argo CD Application, so the change takes effect immediately instead of waiting for Argo's next reconciliation.

This is the first "bot" implementation that takes a different approach than the original Bridge design. We expect to replace all bridge functionality with this kind of bot mechanism.

## How it works

argobot connects as a ConfigHub worker and reacts to events over the long-poll connection:

- It subscribes to the `apply.completed` and `release.published` event types, optionally scoped to a single Space or Target. The subscription is declared when argobot connects.
- On a delivered event it resolves which Argo CD Application to sync (see below) and hard-refreshes it via the Argo CD REST API. For an OCI source this forces a tag→digest re-resolve rather than a plain sync, or Argo would replay the cached digest and miss the freshly published bundle. The reaction is argobot's own; the event only says the desired state for a (Space, Target) changed.
- Argo CD credentials are argobot's own authority, supplied by environment, never brokered through ConfigHub.
- The delivery cursor is held by ConfigHub, keyed by the worker and the subscription name, so a restart resumes where it left off without argobot keeping any local state.

Resolving the Application. When `ARGO_APP` is set, it is the single Application every matching event force-syncs — the simplest deployment. When it is not set, a `release.published` event carries its Space slug, which by the app-name == space-slug convention names the Application; an `apply.completed` event without `ARGO_APP` cannot be resolved and is skipped with a warning.

Correctness belongs to Argo CD: it reconciles from its source regardless, so a missed or failed sync only loses immediacy, not correctness.

### Trigger model

The event subscription is the primary trigger. A bridge (`Argo` provider) also stays registered, so the imperative path still works: applying a Unit bound to an Argo target force-syncs it directly, reading the target's `BridgeHandle` as the Application name plus the `AppNamespace` / `Prune` / `Force` options. That path is the secondary, human-driven trigger; the event subscription is what makes a deploy take effect on its own.

The reporting direction — argobot pushing Argo / Kubernetes health back into ConfigHub over the REST API — is the next step and not yet built.

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
| `CONFIGHUB_SUBSCRIPTION_NAME` | no | Subscription name; keys the server-held delivery cursor, so it must be stable across restarts. Defaults to `argobot`. |
| `CONFIGHUB_EVENT_SPACE_ID` | no | Scope delivery to one Space (UUID). Empty means every Space. |
| `CONFIGHUB_EVENT_TARGET_ID` | no | Scope delivery to one Target (UUID). Empty means every Target. |
| `ARGO_APP` | no | The single Argo CD Application every matching event force-syncs. If unset, the Application is resolved from the event (a release carries its Space slug). |
| `ARGO_APP_NAMESPACE` | no | Argo CD Application namespace passed to the sync request. |
| `ARGO_PRUNE` | no | `true` to prune on sync. |
| `ARGO_FORCE` | no | `true` to use Argo's force strategy on sync. |

## Setup

Install the `cub` CLI (see [docs.confighub.com](https://docs.confighub.com)), then:

```sh
# 1. Create the worker (bot identity) in the space that owns the Argo targets.
cub worker create argobot --space my-space

# 2. Read its ID and secret for argobot's environment.
cub worker get argobot --space my-space
```

Then run argobot with the worker credentials and its Argo CD credentials, pointing it at the Application to sync:

```sh
CONFIGHUB_URL=https://hub.confighub.com \
CONFIGHUB_WORKER_ID=... CONFIGHUB_WORKER_SECRET=... \
ARGOCD_SERVER=https://argocd.example.com ARGOCD_AUTH_TOKEN=... \
ARGO_APP=my-app \
argobot
```

A `cub unit apply` or a `cub release` for the scoped (Space, Target) now emits an event that argobot receives and force-syncs on. To limit which deploys argobot reacts to, set `CONFIGHUB_EVENT_SPACE_ID` or `CONFIGHUB_EVENT_TARGET_ID`.

The imperative path also remains: create an `Argo` target bound to the worker (its `BridgeHandle` is the Application name) and apply a Unit on it to force-sync directly, without an event subscription.

(Exact `cub` flags may vary by CLI version; `cub --help`.)

## Deploy

argobot is meant to run in the cluster alongside Argo CD, with its own manifests managed in ConfigHub. `deploy/argobot.yaml` is a ready-to-store Unit: a ConfigMap for non-secret settings and a Deployment. Create the `argobot-secrets` Secret (`CONFIGHUB_WORKER_ID`, `CONFIGHUB_WORKER_SECRET`, `ARGOCD_AUTH_TOKEN`) out of band or via your secret store.

## Build

```sh
go build ./...        # compile
docker build -t ghcr.io/confighub/argobot:dev .
```

CI (`.github/workflows/build.yml`) builds and pushes the image to `ghcr.io/confighub/argobot` on pushes to `main` and version tags.
