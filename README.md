# argobot

A ConfigHub bot that syncs Argo CD applications, or Sveltos profiles.

argobot listens on a ConfigHub Channel that subscribes to ConfigHub's event log. When ConfigHub records that a Release was published, argobot triggers a sync of the corresponding Argo CD Application (or, with `GITOPS_CONTROLLER=sveltos`, a redeploy of the Sveltos profile; see [Sveltos](#sveltos)), so the change takes effect immediately instead of waiting for the controller's next reconciliation.

This is a "bot" in the sense ConfigHub's Channels give the word: a process that claims a mailbox ConfigHub holds for it, rather than a Bridge worker. It is the pattern the Bridge is being replaced with.

## How it works

argobot claims a ConfigHub Channel and polls it:

- The Channel is created out of band with `cub channel create`, in the Space of the cluster argobot serves, and subscribes to the `release.published` event type. What the Channel subscribes to is its scope: by default every Space and Target, narrowed on the Channel (a `SpaceID` or `TargetID` on a subscription) with `cub channel update`, never in argobot's own configuration. `argobot --print-subscriptions` prints the default.
- argobot authenticates as a ConfigHub service account, by signing a short-lived assertion with a private key it holds (ConfigHub keeps only the public half), and claims the Channel, which that service account must hold `Claim` on. One process holds the lease at a time; a second replica waits for it.
- On a delivered event it resolves which Argo CD Application to sync (see below) and triggers a sync. The reaction is argobot's own; the event only says the desired state for a (Space, Target) changed. An event of another type, which a Channel subscribed more widely would deliver, is logged and skipped.
- The Channel bookmarks how far it has read, so a restart resumes where it left off without argobot keeping any local state. A new Channel starts at the tail of the log.

### Sync modes

`ARGO_SYNC_MODE` selects how argobot triggers the sync:

- **`kubernetes`** (default) — argobot patches the Application's `argocd.argoproj.io/refresh` annotation through the Kubernetes API. Argo re-compares the Application against its source (a **hard** refresh re-resolves the source, including an OCI tag→digest, so Argo picks up a freshly published bundle rather than replaying a cached digest) and then, **if the Application has auto-sync enabled, deploys**. If auto-sync is off, argobot's refresh does not deploy — auto-sync is the deploy gate. Credentials are the pod's ServiceAccount; there is no Argo CD API token to set up.
- **`argocd`** — argobot calls the Argo CD REST `/api/v1/applications/{name}/sync` endpoint. This deploys **regardless** of the Application's auto-sync setting. It needs an Argo CD API base URL and bearer token, which are argobot's own authority, supplied by environment and never brokered through ConfigHub.

Resolving the Application. When `ARGO_APP` is set, it is the single Application every matching event syncs — the simplest deployment. When it is not set, the event's Space slug is resolved to every Application in `ARGO_NAMESPACE` whose OCI source names that Space — a `spec.source.repoURL` or any `spec.sources[].repoURL` ending in `/space/<slug>` — plus the Application named `<slug>`, by the app-name == space-slug convention. Each Application is synced once. Matching by source finds Applications that kept the name Argo CD gave them when `cub argo` moved them onto ConfigHub (for example Application `dev-1-apptique` reading `oci://gw/space/argo-apptique-dev-1`). If no Application is found, argobot logs a warning naming the slug and skips the event. argobot lists the Applications on each event (`list` on Application resources in `kubernetes` mode; `GET /api/v1/applications`, scoped to `ARGO_APP_NAMESPACE` when set, in `argocd` mode); if the list fails it logs a warning and falls back to the Application named `<slug>`.

Correctness belongs to Argo CD: it reconciles from its source regardless, so a missed or failed sync only loses immediacy, not correctness.

### Live-status reporting

Alongside reacting to events, argobot reports the other direction: it watches Argo CD Applications and writes their live state back into ConfigHub, where the UI surfaces it. This is best-effort feedback — a dropped or delayed report costs only freshness, never correctness.

argobot runs a Kubernetes informer over Argo CD Application CRs. Kubernetes watches cannot filter on what an Application deploys, so it watches every Application in the namespace and maps each to the ConfigHub Release it is running, in-process. An Application's OCI source (`.../space/<slug>`), or equivalently its name, names the deployment Space, and the revision Argo CD synced is the `ManifestDigest` of the Release it is running; the newest Release of the Space with that digest is the one. For each mapped Application, argobot writes the Release's `LiveStatus`: sync, health, and operation, normalized to ConfigHub's values with Argo CD's own words kept beside them, and a short message. An Application whose Space or Release does not resolve is ignored.

argobot's access comes from grants on the Target its deployment Spaces release to: View and ViewChildren let it read the Spaces and their Releases, and EditChildren lets it write their `LiveStatus`. `cub cluster up` grants all three to the service account argobot runs as.

Writes are coalesced per Application (a sync's burst of updates becomes one write) and deduplicated against the last projection (an idle Application produces no write). Reporting is on by default and needs Kubernetes access; set `CONFIGHUB_REPORT_LIVE_STATUS=false` to disable it, and if the Kubernetes client cannot be built the reporter is skipped with a warning rather than failing the bot.

### Sveltos

`GITOPS_CONTROLLER=sveltos` makes argobot drive [Sveltos](https://projectsveltos.github.io/sveltos/) instead of Argo CD. Event handling, the Channel and the ConfigHub calls are the same; the two Argo-facing parts change. The `ARGO_*`/`ARGOCD_*` variables are Argo CD only and are ignored.

**Forcing a sync.** argobot writes a new value (the current time) to the `profile.projectsveltos.io/redeploy` annotation of each profile that deploys the event's Space. Any change of value redeploys every ClusterSummary the profile manages, which fetches its `remoteURL` again, so a new Release reaches the cluster within seconds instead of at the profile's next poll; a redeploy with no new Release writes no objects. Unlike Argo's refresh annotation, Sveltos leaves the annotation in place, which is why each sync writes a new value. Sveltos ignores it for profiles with `syncMode: OneTime`. The profiles are the ClusterProfiles (cluster-scoped, named by their name) and Profiles (named `<namespace>/<name>`) whose `spec.policyRefs[].remoteURL.url` or `spec.kustomizationRefs[].remoteURL.url` names the Space — Sveltos keeps the tag in the URL (`.../space/<slug>:latest`), and a trailing `:<tag>` or `@<digest>` is ignored — plus the ClusterProfile named `<slug>`. On a ClusterProfile the annotation redeploys every cluster the profile selects. Under `cub cluster up` the child ClusterProfiles are applied by a root profile; Sveltos keeps argobot's annotation when the root reapplies a child.

**Live status.** Sveltos records no digest of what it fetched, so argobot reads the deployed objects instead. Every object in a Release carries a `confighub.com/origin` annotation naming its Space, Unit and Revision. A `HealthCheck` with `collectResources: true`, run by a `ClusterHealthCheck`, makes Sveltos write a `HealthCheckReport` per cluster holding each collected object and its health. argobot watches HealthCheckReports in every namespace, reads the origin of each collected object, and finds the Release each Space is running: publishing tags every bundled Revision with the Release's Tag, so it is the newest published Release of the Space whose Tag is on every observed Unit revision (withdrawn Releases are skipped; several Releases can match when Units didn't change, and those bundle the same content). Health is the worst of the objects' `healthStatus`; sync and operation come from the cluster's ClusterSummary for the profile that deploys the Space (`Provisioned` is synced, `Provisioning` a running operation, `Failed`/`FailedNonRetriable` a failed one).

- **Coverage.** The HealthCheck must select at least one object of every Unit, or argobot can't tell which revision of an unselected Unit is running and reports nothing for that Space. Reports also have a size limit (a Kubernetes object is capped at about 1.5 MiB): for big Releases, select one small object per Unit rather than everything, or split the check.
- **One report per cluster, one `LiveStatus` per Release.** With one profile per variant per cluster, as `cub cluster up` sets up, these coincide. A Release that several clusters run is written by each cluster's report in turn.

`manifests/sveltos/argobot.yaml` is the Sveltos deployment, for the Sveltos management cluster: a `ClusterRole` to `patch` ClusterProfiles and Profiles, the read-only `ClusterRole` extended to the `config.projectsveltos.io` and `lib.projectsveltos.io` API groups, a `HealthCheck` and `ClusterHealthCheck` that collect every object carrying `confighub.com/origin` across common kinds (extend `resourceSelectors` to cover the kinds your Releases contain), and the Deployment with `GITOPS_CONTROLLER=sveltos`. It is not part of the published config bundle.

## Configuration

All configuration is via environment variables:

| Variable | Required | Description |
|---|---|---|
| `CONFIGHUB_URL` | yes | ConfigHub base URL, e.g. `https://hub.confighub.com` |
| `CONFIGHUB_AUTH_PRIVATE_KEY` | yes | The service account key argobot authenticates with, as a private JWK (the file `cub serviceaccount key add --generate` writes) |
| `CONFIGHUB_CHANNEL` | yes | The Channel argobot claims and polls, as `<space>/<slug>` or a UUID |
| `CONFIGHUB_ASSERTION_AUDIENCE` | no | Only for a ConfigHub server that sets `CONFIGHUB_ASSERTION_AUDIENCE` to something other than its default; must match it. |
| `GITOPS_CONTROLLER` | no | `argocd` (default) or `sveltos`. See [Sveltos](#sveltos). Every `ARGO_*`/`ARGOCD_*` variable below applies to `argocd` only. |
| `ARGO_SYNC_MODE` | no | `kubernetes` (default) or `argocd`. See [Sync modes](#sync-modes). |
| `ARGO_NAMESPACE` | no | _kubernetes mode._ Namespace holding the Argo CD Application resources. Defaults to `argocd`. |
| `ARGO_REFRESH_TYPE` | no | _kubernetes mode._ `hard` (default) or `normal`. |
| `ARGOCD_SERVER` | argocd mode | Argo CD API base URL, e.g. `https://argocd.example.com` |
| `ARGOCD_AUTH_TOKEN` | argocd mode | Argo CD API bearer token |
| `ARGOCD_INSECURE` | no | _argocd mode._ `true` to skip TLS verification (self-signed Argo CD) |
| `ARGO_APP` | no | The single Argo CD Application every matching event syncs. If unset, the event's Space slug resolves to every Application whose OCI source names that Space, plus the one named after it. |
| `ARGO_APP_NAMESPACE` | no | _argocd mode._ Argo CD Application namespace passed to the REST sync request (apps-in-any-namespace). |
| `ARGO_PRUNE` | no | _argocd mode._ `true` to prune on sync. |
| `ARGO_FORCE` | no | _argocd mode._ `true` to use Argo's force strategy on sync. |
| `CONFIGHUB_REPORT_LIVE_STATUS` | no | `false` to disable live-status reporting. On by default; see [Live-status reporting](#live-status-reporting). Needs Kubernetes access. |

When `GITOPS_CONTROLLER` is unset, argobot drives Argo CD. When `ARGO_SYNC_MODE` is unset, argobot runs in `kubernetes` mode and needs no Argo CD credentials; it uses the in-cluster ServiceAccount (or a local kubeconfig when run out of cluster). Set `ARGO_SYNC_MODE=argocd` to use the REST API instead, which then requires `ARGOCD_SERVER` and `ARGOCD_AUTH_TOKEN`.

Which events argobot receives is not configured here. It is the Channel's `Subscriptions`, set with `cub channel create --subscriptions` or `cub channel update`. The variables an earlier argobot read as a worker (`CONFIGHUB_WORKER_ID`, `CONFIGHUB_WORKER_SECRET`, and the subscription scope `CONFIGHUB_SUBSCRIPTION_NAME`, `CONFIGHUB_EVENT_SPACE_ID`, `CONFIGHUB_EVENT_TARGET_ID`) are ignored with a warning.

## Setup

Install the `cub` CLI (see [docs.confighub.com](https://docs.confighub.com)), then:

```sh
# 1. Create the service account argobot runs as, and a key for it. The private
#    half lands in ~/.confighub/keys/argobot-my-cluster.jwk; it is what argobot
#    gets as CONFIGHUB_AUTH_PRIVATE_KEY.
cub serviceaccount create argobot-my-cluster
cub serviceaccount key add argobot-my-cluster --generate argobot-my-cluster

# 2. Grant it the cluster's Target: View and ViewChildren to read the Spaces
#    released to it, EditChildren to write their live status.
cub target update target --space my-cluster \
  --permission View:serviceaccount:argobot-my-cluster \
  --permission ViewChildren:serviceaccount:argobot-my-cluster \
  --permission EditChildren:serviceaccount:argobot-my-cluster

# 3. Create the Channel beside the cluster's Target, subscribed to the apply and
#    release events, and let the service account claim it.
argobot --print-subscriptions | cub channel create argobot --space my-cluster \
  --subscriptions - --permission Claim:serviceaccount:argobot-my-cluster
```

A Channel created this way subscribes to deploys on every Space and Target. To react only to one cluster's deploys, scope the subscription to its Target with `cub channel update argobot --space my-cluster --subscriptions -`, giving the same JSON with a `TargetID`.

Then run argobot with the key, naming the Channel. In the default `kubernetes` mode it uses the ambient Kubernetes credentials (in-cluster ServiceAccount, or your local kubeconfig) — no Argo CD token:

```sh
CONFIGHUB_URL=https://hub.confighub.com \
CONFIGHUB_AUTH_PRIVATE_KEY="$(cat ~/.confighub/keys/argobot-my-cluster.jwk)" \
CONFIGHUB_CHANNEL=my-cluster/argobot \
ARGO_NAMESPACE=argocd \
ARGO_APP=my-app \
argobot
```

To use the Argo CD REST API instead (deploys regardless of auto-sync), set `ARGO_SYNC_MODE=argocd` and supply its credentials:

```sh
CONFIGHUB_URL=https://hub.confighub.com \
CONFIGHUB_AUTH_PRIVATE_KEY="$(cat ~/.confighub/keys/argobot-my-cluster.jwk)" \
CONFIGHUB_CHANNEL=my-cluster/argobot \
ARGO_SYNC_MODE=argocd \
ARGOCD_SERVER=https://argocd.example.com ARGOCD_AUTH_TOKEN=... \
ARGO_APP=my-app \
argobot
```

A `cub unit apply` or a `cub release publish` for a (Space, Target) the Channel subscribes to now emits an event that argobot receives and syncs on. `cub channel get argobot --space my-cluster` shows whether argobot holds the lease and when it last polled.

(Exact `cub` flags may vary by CLI version; `cub --help`.)

## Deploy

argobot runs in the cluster alongside Argo CD. `manifests/argobot.yaml` is a complete, self-documenting deployment: a Namespace, a ServiceAccount, its RBAC (see below), and a Deployment whose `env` list carries **every** supported variable — defaults filled in, and the ones that must be absent by default commented out. The Deployment reads its credentials from a Secret named `argobot-secrets` via `secretKeyRef`; that Secret is intentionally **not** part of the manifest — supply it out of band (secret store / External Secrets, or `kubectl create secret generic argobot-secrets --from-literal=...`) so the bundle never ships or overwrites credential material. Expected keys: `CONFIGHUB_AUTH_PRIVATE_KEY` (the private JWK) and (argocd mode only) `ARGOCD_AUTH_TOKEN`.

RBAC comes in two parts. A **`Role`/`RoleBinding` in the Argo CD namespace** (`argocd` by default) grants read+write there — argobot patches an Application's refresh annotation to force a sync, and may manage other Argo CD resources; being namespaced, it grants nothing outside `argocd`. A **`ClusterRole`/`ClusterRoleBinding`** grants cluster-wide **read-only** (`get`/`list`/`watch`) for health/status reporting across all namespaces — deliberately **excluding Secrets** (the core API group is enumerated without `secrets`; other groups are wildcarded, and Secrets exist only in core). Add a CRD's API group to that second rule to extend coverage. The Namespace, ServiceAccount, and Deployment are argobot's own — adjust the namespaces to match your cluster.

For Sveltos, deploy `manifests/sveltos/argobot.yaml` instead; see [Sveltos](#sveltos).

### Config bundle

`.github/workflows/release.yml` packages `manifests/` into an OCI bundle and pushes it to `ghcr.io/confighub/configs/argobot:latest`. The bundle **floats** — a single moving `:latest` — but each cut pins a concrete released image: the committed manifest carries `argobot:latest`, and the workflow substitutes it with the current released version (resolved from git tags) before publishing, failing the build if the pin does not take. Load it into ConfigHub as a component base:

```sh
cub variant upload --component argobot --variant base --granularity per-file \
  oci://ghcr.io/confighub/configs/argobot
```

## Upgrading from 0.1

argobot 0.1 connected as a Bridge worker and chose its own subscriptions. 0.2 listens on a Channel as a service account and reads none of the worker variables, so the two need different ConfigHub objects and a different Secret. The order below keeps the 0.1 argobot running until the 0.2 one works, and every step but the last is reversible. `my-cluster` is the Space holding the Target the worker is granted on.

```sh
# 1. The identity: a service account, a key for argobot, and a token for
#    Argo CD if it pulls from ConfigHub's OCI registry with the worker's
#    credentials. The token is printed once.
cub serviceaccount create argobot-my-cluster
cub serviceaccount key add argobot-my-cluster --generate argobot-my-cluster
cub serviceaccount token create argobot-my-cluster

# 2. The grants the worker has, now for the service account too. The worker
#    keeps its own.
cub target update target --space my-cluster \
  --permission View:serviceaccount:argobot-my-cluster \
  --permission ViewChildren:serviceaccount:argobot-my-cluster \
  --permission EditChildren:serviceaccount:argobot-my-cluster

# 3. The Channel. --print-subscriptions needs the 0.2 binary; run it from the
#    image. If the 0.1 argobot had CONFIGHUB_EVENT_TARGET_ID set, scope the
#    subscriptions to that Target as described under Setup.
docker run --rm ghcr.io/confighub/argobot:latest --print-subscriptions \
  | cub channel create argobot --space my-cluster \
      --subscriptions - --permission Claim:serviceaccount:argobot-my-cluster
```

4. **Argo CD's pull credentials.** If Argo CD's repo-creds Secret for the ConfigHub registry carries the worker ID and secret, set its `username` to `token` and its `password` to the token from step 1. Argo CD reads the Secret at its next pull, and the token is already valid, so no pull fails.
5. **argobot's Secret.** Add `CONFIGHUB_AUTH_PRIVATE_KEY`, the contents of the `.jwk` file from step 1, to `argobot-secrets`. The 0.1 argobot ignores it.
6. **The rollout.** Move the Deployment to the 0.2 image, set `CONFIGHUB_CHANNEL=my-cluster/argobot`, and drop the worker and subscription variables (left in place, they only warn). A one-replica Deployment keeps the 0.1 pod until the 0.2 pod is Ready, so a missing key or Claim shows up as a crash-looping new pod beside a working old one, and reverting the Deployment undoes it. `cub channel get argobot --space my-cluster` shows when the 0.2 argobot holds the Channel.
7. **The worker.** Once the Channel is held, delete the worker with `cub worker delete`. This is the one step the rest cannot undo.

A kind cluster from `cub cluster up` of an earlier cub is simpler to recreate than to upgrade: `cub cluster down --name <name> --delete-config`, then `cub cluster up --name <name>` with the new cub, which creates the service account and the Channel itself. `--delete-config` also deletes the deployment Spaces bound to that cluster.

## Build and release

```sh
go build ./...        # compile
docker build -t ghcr.io/confighub/argobot:dev .
```

Two workflows, two cadences:

- `.github/workflows/build.yml` builds and pushes **development** images to `ghcr.io/confighub/argobot` on pushes to `main` and on PRs (tags `main`, `pr-N`, `sha-…`). No semver, no `:latest`.
- `.github/workflows/release.yml` cuts a **release** when you push a `vX.Y.Z` git tag: it builds the semver image (`:vX.Y.Z`, plus `:latest`) and republishes the config bundle pinned to it. The bundle is also republished on every `main` push, pinned to the current latest tag, so a config-only change ships without a new image. The pinned version is never committed, so there is no commit-to-release cycle.

Every image carries its third-party notices at `/app/THIRD_PARTY_LICENSES.txt` (Go modules linked into the binary) and `/app/OS_LICENSE_NOTICE.txt` (Alpine base packages, with the GPL source offer), and every published manifest has an SPDX SBOM attestation (`docker buildx imagetools inspect ghcr.io/confighub/argobot:<tag> --format '{{json .SBOM}}'`). After changing dependencies, run `./scripts/gen-third-party-licenses.sh` and commit the result; `.github/workflows/third-party-licenses.yml` fails the PR if the committed file is stale or a copyleft dependency is linked in.
