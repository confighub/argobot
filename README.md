# argobot

A ConfigHub bot that syncs Argo CD applications, Sveltos profiles, or Flux OCI sources.

argobot listens on a ConfigHub Channel that subscribes to ConfigHub's event log. When ConfigHub records that a Release was published, argobot triggers a sync of the corresponding Argo CD Application (or, with `GITOPS_CONTROLLER=sveltos`, a redeploy of the Sveltos profile, see [Sveltos](#sveltos); or with `GITOPS_CONTROLLER=flux`, a reconciliation of the Flux OCIRepository, see [Flux](#flux)), so the change takes effect immediately instead of waiting for the controller's next reconciliation.

This is a "bot" in the sense ConfigHub's Channels give the word: a process that claims a mailbox ConfigHub holds for it, rather than a Bridge worker. It is the pattern the Bridge is being replaced with.

## How it works

argobot claims a ConfigHub Channel and polls it:

- The Channel is created out of band with `cub channel create`, in the Space of the cluster argobot serves, and subscribes to the `release.published` event type. What the Channel subscribes to is its scope: by default every Space and Target, narrowed on the Channel (a `SpaceID` or `TargetID` on a subscription) with `cub channel update`, never in argobot's own configuration. `argobot --print-subscriptions` prints the default.
- argobot authenticates as a ConfigHub service account, by signing a short-lived assertion with a private key it holds (ConfigHub keeps only the public half), and claims the Channel, which that service account must hold `Claim` on. One process holds the lease at a time; a second replica waits for it.
- On a delivered event it resolves which Argo CD Application to sync (see below) and triggers a sync. The reaction is argobot's own; the event only says the desired state for a (Space, Target) changed. An event of another type, which a Channel subscribed more widely would deliver, is logged and skipped.
- The Channel bookmarks how far it has read, so a restart resumes where it left off without argobot keeping any local state. A new Channel starts at the tail of the log.

Forcing a sync. argobot patches the Application's `argocd.argoproj.io/refresh` annotation through the Kubernetes API. Argo re-compares the Application against its source (a **hard** refresh re-resolves the source, including an OCI tag→digest, so Argo picks up a freshly published bundle rather than replaying a cached digest) and then, **if the Application has auto-sync enabled, deploys**. If auto-sync is off, argobot's refresh does not deploy — auto-sync is the deploy gate. Credentials are the pod's ServiceAccount; there is no Argo CD API token to set up.

Resolving the Application. When `ARGO_APP` is set, it is the single Application every matching event syncs — the simplest deployment. When it is not set, the event's Space slug is resolved to every Application in `ARGO_NAMESPACE` whose OCI source names that Space — a `spec.source.repoURL` or any `spec.sources[].repoURL` ending in `/space/<slug>` — plus the Application named `<slug>`, by the app-name == space-slug convention. Each Application is synced once. Matching by source finds Applications that kept the name Argo CD gave them when `cub argo` moved them onto ConfigHub (for example Application `dev-1-apptique` reading `oci://gw/space/argo-apptique-dev-1`). If no Application is found, argobot logs a warning naming the slug and skips the event. argobot lists the Applications in `ARGO_NAMESPACE` through the Kubernetes API on each event; if the list fails it logs a warning and falls back to the Application named `<slug>`.

Correctness belongs to Argo CD: it reconciles from its source regardless, so a missed or failed sync only loses immediacy, not correctness.

### Live-status reporting

Alongside reacting to events, argobot reports the other direction: it watches Argo CD Applications and writes their live state back into ConfigHub, where the UI surfaces it. This is best-effort feedback — a dropped or delayed report costs only freshness, never correctness.

argobot runs a Kubernetes informer over Argo CD Application CRs. Kubernetes watches cannot filter on what an Application deploys, so it watches every Application in the namespace and maps each to the ConfigHub Release it is running, in-process. An Application's OCI source (`.../space/<slug>`), or equivalently its name, names the deployment Space, and the revision Argo CD synced is the `ManifestDigest` of the Release it is running; the newest Release of the Space with that digest is the one. For each mapped Application, argobot writes the Release's `LiveStatus`: sync, health, and operation, normalized to ConfigHub's values with Argo CD's own words kept beside them, and a short message. An Application whose Space or Release does not resolve is ignored.

argobot's access comes from grants on the Target its deployment Spaces release to: View and ViewChildren let it read the Spaces and their Releases, EditChildren lets it write their `LiveStatus`, and Edit lets it store [cluster facts](#cluster-facts) on the Target itself. `cub cluster up` grants all four to the service account argobot runs as.

Writes are coalesced per Application (a sync's burst of updates becomes one write) and deduplicated against the last projection (an idle Application produces no write). Reporting is on by default and needs Kubernetes access; set `CONFIGHUB_REPORT_LIVE_STATUS=false` to disable it, and if the Kubernetes client cannot be built the reporter is skipped with a warning rather than failing the bot.

### Cluster facts

When `CONFIGHUB_TARGET` names the Target that represents the cluster, argobot collects facts about the cluster at startup and stores them on that Target, as `cub k8s collect` does: the Kubernetes version, the CRDs and built-in resource types it serves (by scope, as `group/version/Kind`), its storage classes and default storage class, and its ingress classes. They are kept under the reserved `Cluster.` prefix, which collection owns: argobot rewrites those facts and removes the ones the cluster no longer has, and leaves every fact outside the prefix alone. `Cluster.Name` cannot be discovered from the cluster, so argobot leaves it as it was (`cub k8s collect --cluster-name` sets it). The facts are then available to `where` filters, e.g. `Facts.Cluster.KubernetesVersion = '1.31.2'`.

Collecting needs Kubernetes discovery and `list` on CustomResourceDefinitions, StorageClasses and IngressClasses, which the read-only `ClusterRole` grants, and Edit on the Target. Like live-status reporting, it is best-effort: if it fails, argobot logs a warning and goes on syncing. It runs once per start, so a cluster upgrade reaches the Target's facts when argobot next restarts. The facts describe the cluster argobot runs in, which under Sveltos is the management cluster. `cub cluster up` sets `CONFIGHUB_TARGET` to the cluster's Target.

### Sveltos

`GITOPS_CONTROLLER=sveltos` makes argobot drive [Sveltos](https://projectsveltos.github.io/sveltos/) instead of Argo CD. Event handling, the Channel and the ConfigHub calls are the same; the two Argo-facing parts change. The `ARGO_*` variables are Argo CD only and are ignored.

**Forcing a sync.** argobot writes a new value (the current time) to the `profile.projectsveltos.io/redeploy` annotation of each profile that deploys the event's Space. Any change of value redeploys every ClusterSummary the profile manages, which fetches its `remoteURL` again, so a new Release reaches the cluster within seconds instead of at the profile's next poll; a redeploy with no new Release writes no objects. Unlike Argo's refresh annotation, Sveltos leaves the annotation in place, which is why each sync writes a new value. Sveltos ignores it for profiles with `syncMode: OneTime`. The profiles are the ClusterProfiles (cluster-scoped, named by their name) and Profiles (named `<namespace>/<name>`) whose `spec.policyRefs[].remoteURL.url` or `spec.kustomizationRefs[].remoteURL.url` names the Space — Sveltos keeps the tag in the URL (`.../space/<slug>:latest`), and a trailing `:<tag>` or `@<digest>` is ignored — plus the ClusterProfile named `<slug>`. On a ClusterProfile the annotation redeploys every cluster the profile selects. Under `cub cluster up` the child ClusterProfiles are applied by a root profile; Sveltos keeps argobot's annotation when the root reapplies a child.

**Live status.** Sveltos records no digest of what it fetched, so argobot reads the deployed objects instead. Every object in a Release carries a `confighub.com/origin` annotation naming its Space, Unit and Revision. A `HealthCheck` with `collectResources: true`, run by a `ClusterHealthCheck`, makes Sveltos write a `HealthCheckReport` per cluster holding each collected object and its health. argobot watches HealthCheckReports in every namespace, reads the origin of each collected object, and finds the Release each Space is running: publishing tags every bundled Revision with the Release's Tag, so it is the newest published Release of the Space whose Tag is on every observed Unit revision (withdrawn Releases are skipped; several Releases can match when Units didn't change, and those bundle the same content). Health is the worst of the objects' `healthStatus`; sync and operation come from the cluster's ClusterSummary for the profile that deploys the Space (`Provisioned` is synced, `Provisioning` a running operation, `Failed`/`FailedNonRetriable` a failed one).

- **Coverage.** The HealthCheck must select at least one object of every Unit, or argobot can't tell which revision of an unselected Unit is running and reports nothing for that Space. Reports also have a size limit (a Kubernetes object is capped at about 1.5 MiB): for big Releases, select one small object per Unit rather than everything, or split the check.
- **One report per cluster, one `LiveStatus` per Release.** With one profile per variant per cluster, as `cub cluster up` sets up, these coincide. A Release that several clusters run is written by each cluster's report in turn.

`manifests/sveltos/argobot.yaml` is the Sveltos deployment, for the Sveltos management cluster: a `ClusterRole` to `patch` ClusterProfiles and Profiles, the read-only `ClusterRole` extended to the `config.projectsveltos.io` and `lib.projectsveltos.io` API groups, a `HealthCheck` and `ClusterHealthCheck` that collect every object carrying `confighub.com/origin` across common kinds (extend `resourceSelectors` to cover the kinds your Releases contain), and the Deployment with `GITOPS_CONTROLLER=sveltos`.

### Flux

`GITOPS_CONTROLLER=flux` makes argobot drive [Flux](https://fluxcd.io/) (2.6 or later) instead of Argo CD. As with Sveltos, the Channel, event handling and the ConfigHub calls are the same, and the `ARGO_*` variables are ignored. A Space is deployed by an `OCIRepository` pulling `oci://<gateway>/space/<slug>` with `ref.tag: latest` (the gateway serves only `latest` or a digest; `ref.semver` won't resolve) and a pull secret whose username is `token` and password a ConfigHub token, plus a `Kustomization` applying it. No `layerSelector` is needed: a Release is a single tar+gzip layer, which Flux extracts by default.

**Forcing a sync.** argobot writes a new value (the current time) to the `reconcile.fluxcd.io/requestedAt` annotation of each OCIRepository that pulls the event's Space. source-controller resolves the tag again; if it points at a new Release, it stores the new artifact and kustomize-controller reconciles every Kustomization that applies the repository, so the Release reaches the cluster within seconds instead of at the repository's next `interval`. Like Sveltos, Flux leaves the annotation in place, which is why each sync writes a new value. The repositories are the OCIRepositories (named `<namespace>/<name>`) whose `spec.url` names the Space, plus the one named `<slug>`. If listing them fails, argobot tries `<slug>` in `FLUX_NAMESPACE`.

**Live status.** Flux records the digests it fetched and applied, as Argo CD does: a Kustomization's `lastAppliedRevision` and `lastAttemptedRevision` are `<tag>@<digest>`, and each digest is a Release's `ManifestDigest`. argobot watches OCIRepositories and Kustomizations in every namespace, and writes each fact to the Release it is about, since a Release's `LiveStatus` describes that Release:

- **The Release the cluster runs** gets the Kustomization's state: `Synced` once applied, health from the `Healthy` and `Ready` conditions, and operation `Failed` when `Ready=False` or `Stalled=True`, `Running` while `Reconciling=True`, `Succeeded` when `Ready=True`. Flux advances `lastAppliedRevision` only once a revision passes its health check, so a revision that was applied and is being checked (`Healthy=Unknown` while `Reconciling=True`), or failed the check (`HealthCheckFailed`), counts as running.
- **A newer Release the Kustomization attempted but did not apply** (it failed to build or apply, or is on its way) gets `OutOfSync` with the operation `Failed` or `Running` and Flux's message. The Release still running meanwhile stays `Synced`.
- **A Release the OCIRepository failed to pull** gets `OutOfSync`/`Failed` with the repository's message. That's the Space's newest published Release, which `latest` resolves to, or the pinned `ref.digest`, unless the cluster already runs or attempts it.
- **A Release no longer run or attempted** is marked replaced (`OutOfSync`, message `Replaced by release N`), once argobot has reported on it for this repository (its `DataSource` names one of the repository's Kustomizations, or the repository) and something else runs. A Release that had failed keeps the operation `Failed`, Flux's reason, and its message after `Replaced by release N, after it failed:`. A published Release older than the one running that nothing reported on, such as one replaced before it was applied, is marked too. A Release already marked is left as is, apart from a `DataSource` an earlier version wrote without the Kustomization's kind, which is rewritten.

The Kustomizations that apply one repository and speak about the same Release are combined into the least advanced state and written once. Each write is compared with the status the Release already has in ConfigHub, so an unchanged state, or a restart, writes nothing.

- **Health checks.** Set `wait: true` (or `healthChecks`) on the Kustomization. Without one, Flux never judges the health of what it applied, and argobot reports health `Unknown`.
- **One `LiveStatus` per Release.** Two OCIRepositories pulling the same Space, or one Release that several clusters run, write the same Release in turn. Clusters whose Kustomizations share a namespace and name, deploying one Space, would mark each other's Releases replaced.

`manifests/flux/argobot.yaml` is the Flux deployment: a `ClusterRole` to `patch` OCIRepositories, the read-only `ClusterRole` extended to the `source.toolkit.fluxcd.io` and `kustomize.toolkit.fluxcd.io` API groups, the Deployment with `GITOPS_CONTROLLER=flux`, and an example OCIRepository and Kustomization in its comments.

## Configuration

All configuration is via environment variables:

| Variable | Required | Description |
|---|---|---|
| `CONFIGHUB_URL` | yes | ConfigHub base URL, e.g. `https://hub.confighub.com` |
| `CONFIGHUB_AUTH_PRIVATE_KEY` | yes | The service account key argobot authenticates with, as a private JWK (the file `cub serviceaccount key add --generate` writes) |
| `CONFIGHUB_CHANNEL` | yes | The Channel argobot claims and polls, as `<space>/<slug>` or a UUID |
| `CONFIGHUB_TARGET` | no | The Target that represents the cluster, as `<space>/<slug>` or a UUID. When set, argobot stores facts about the cluster on it at startup; see [Cluster facts](#cluster-facts). Needs Edit on the Target. |
| `CONFIGHUB_ASSERTION_AUDIENCE` | no | Only for a ConfigHub server that sets `CONFIGHUB_ASSERTION_AUDIENCE` to something other than its default; must match it. |
| `GITOPS_CONTROLLER` | no | `argocd` (default), `sveltos` or `flux`. See [Sveltos](#sveltos) and [Flux](#flux). Every `ARGO_*` variable below applies to `argocd` only. |
| `FLUX_NAMESPACE` | no | _flux only._ Namespace of the OCIRepository named after a Space, tried when listing OCIRepositories fails. Defaults to `flux-system`. |
| `ARGO_NAMESPACE` | no | Namespace holding the Argo CD Application resources. Defaults to `argocd`. |
| `ARGO_REFRESH_TYPE` | no | `hard` (default) or `normal`. |
| `ARGO_APP` | no | The single Argo CD Application every matching event syncs. If unset, the event's Space slug resolves to every Application whose OCI source names that Space, plus the one named after it. |
| `CONFIGHUB_REPORT_LIVE_STATUS` | no | `false` to disable live-status reporting. On by default; see [Live-status reporting](#live-status-reporting). Needs Kubernetes access. |

An empty variable is the same as an unset one. `argobot docgen env` prints these variables as a JSON Schema, with each one's description, default and allowed values; the installer package validates argobot's environment against it.

When `GITOPS_CONTROLLER` is unset, argobot drives Argo CD. It needs no Argo CD credentials: it uses the in-cluster ServiceAccount (or a local kubeconfig when run out of cluster).

Which events argobot receives is not configured here. It is the Channel's `Subscriptions`, set with `cub channel create --subscriptions` or `cub channel update`. The variables an earlier argobot read as a worker (`CONFIGHUB_WORKER_ID`, `CONFIGHUB_WORKER_SECRET`, and the subscription scope `CONFIGHUB_SUBSCRIPTION_NAME`, `CONFIGHUB_EVENT_SPACE_ID`, `CONFIGHUB_EVENT_TARGET_ID`) are ignored with a warning, as are those of the retired Argo CD REST sync mode (`ARGO_SYNC_MODE`, `ARGOCD_SERVER`, `ARGOCD_AUTH_TOKEN`, `ARGOCD_INSECURE`, `ARGO_APP_NAMESPACE`, `ARGO_PRUNE`, `ARGO_FORCE`). argobot now always syncs through the Kubernetes API, which deploys only when the Application has auto-sync enabled.

## Setup

Install the `cub` CLI (see [docs.confighub.com](https://docs.confighub.com)), then:

```sh
# 1. Create the service account argobot runs as, and a key for it. The private
#    half lands in ~/.confighub/keys/argobot-my-cluster.jwk; it is what argobot
#    gets as CONFIGHUB_AUTH_PRIVATE_KEY.
cub serviceaccount create argobot-my-cluster
cub serviceaccount key add argobot-my-cluster --generate argobot-my-cluster

# 2. Grant it the cluster's Target: View and ViewChildren to read the Spaces
#    released to it, EditChildren to write their live status, and Edit to
#    store the cluster's facts on the Target.
cub target update target --space my-cluster \
  --permission View:serviceaccount:argobot-my-cluster \
  --permission Edit:serviceaccount:argobot-my-cluster \
  --permission ViewChildren:serviceaccount:argobot-my-cluster \
  --permission EditChildren:serviceaccount:argobot-my-cluster

# 3. Create the Channel beside the cluster's Target, subscribed to the apply and
#    release events, and let the service account claim it.
argobot --print-subscriptions | cub channel create argobot --space my-cluster \
  --subscriptions - --permission Claim:serviceaccount:argobot-my-cluster
```

A Channel created this way subscribes to deploys on every Space and Target. To react only to one cluster's deploys, scope the subscription to its Target with `cub channel update argobot --space my-cluster --subscriptions -`, giving the same JSON with a `TargetID`.

Then run argobot with the key, naming the Channel and the cluster's Target. It uses the ambient Kubernetes credentials (in-cluster ServiceAccount, or your local kubeconfig) — no Argo CD token:

```sh
CONFIGHUB_URL=https://hub.confighub.com \
CONFIGHUB_AUTH_PRIVATE_KEY="$(cat ~/.confighub/keys/argobot-my-cluster.jwk)" \
CONFIGHUB_CHANNEL=my-cluster/argobot \
CONFIGHUB_TARGET=my-cluster/target \
ARGO_NAMESPACE=argocd \
ARGO_APP=my-app \
argobot
```

A `cub unit apply` or a `cub release publish` for a (Space, Target) the Channel subscribes to now emits an event that argobot receives and syncs on. `cub channel get argobot --space my-cluster` shows whether argobot holds the lease and when it last polled.

(Exact `cub` flags may vary by CLI version; `cub --help`.)

## Deploy

argobot runs in the cluster alongside Argo CD. `manifests/argobot.yaml` is a complete, self-documenting deployment: a Namespace, a ServiceAccount, its RBAC (see below), and a Deployment whose `env` list carries **every** supported variable — defaults filled in, and the ones that must be absent by default commented out. The Deployment reads its credentials from a Secret named `argobot-secrets` via `secretKeyRef`; that Secret is intentionally **not** part of the manifest — supply it out of band (secret store / External Secrets, or `kubectl create secret generic argobot-secrets --from-literal=...`) so the manifest never ships or overwrites credential material. Its only key is `CONFIGHUB_AUTH_PRIVATE_KEY` (the private JWK).

RBAC comes in two parts. A **`Role`/`RoleBinding` in the Argo CD namespace** (`argocd` by default) grants read+write there — argobot patches an Application's refresh annotation to force a sync, and may manage other Argo CD resources; being namespaced, it grants nothing outside `argocd`. A **`ClusterRole`/`ClusterRoleBinding`** grants cluster-wide **read-only** (`get`/`list`/`watch`) for health/status reporting across all namespaces — deliberately **excluding Secrets** (the core API group is enumerated without `secrets`; other groups are wildcarded, and Secrets exist only in core). Add a CRD's API group to that second rule to extend coverage. The Namespace, ServiceAccount, and Deployment are argobot's own — adjust the namespaces to match your cluster.

For Sveltos, deploy `manifests/sveltos/argobot.yaml` instead; see [Sveltos](#sveltos). For Flux, deploy `manifests/flux/argobot.yaml`; see [Flux](#flux).

### Pinned to a release

Each manifest runs the argobot release it ships with, never `latest`, and leaves `CONFIGHUB_CHANNEL` and `CONFIGHUB_TARGET` as `confighubplaceholder`, for each cluster to set. Each argobot image carries the three of them, as `/manifests/argocd.yaml`, `/manifests/flux.yaml` and `/manifests/sveltos.yaml`, in the image's last layer. `cub cluster attach` reads them from there, for the image's own release, without running the image.

### Installer package

`manifests/package` is an [installer](https://github.com/confighub/installer) package for argobot, published as `oci://ghcr.io/confighub/packages/argobot`. It is a common base plus one component per controller, `argocd` by default. Its inputs are all optional, so an install can be a base that ConfigHub variants are cloned from. As with the manifests, the `argobot-secrets` Secret is not part of it. The Deployment runs a pinned image tag, never `latest`. See [its README](manifests/package/README.md).

## Upgrading from 0.1

argobot 0.1 connected as a Bridge worker and chose its own subscriptions. 0.2 listens on a Channel as a service account and reads none of the worker variables, so the two need different ConfigHub objects and a different Secret. The order below keeps the 0.1 argobot running until the 0.2 one works, and every step but the last is reversible. `my-cluster` is the Space holding the Target the worker is granted on.

```sh
# 1. The identity: a service account, a key for argobot, and a token for
#    Argo CD if it pulls from ConfigHub's OCI registry with the worker's
#    credentials. The token is printed once.
cub serviceaccount create argobot-my-cluster
cub serviceaccount key add argobot-my-cluster --generate argobot-my-cluster
cub serviceaccount token create argobot-my-cluster

# 2. The grants the worker has, now for the service account too, and Edit to
#    store the cluster's facts on the Target. The worker keeps its own.
cub target update target --space my-cluster \
  --permission View:serviceaccount:argobot-my-cluster \
  --permission Edit:serviceaccount:argobot-my-cluster \
  --permission ViewChildren:serviceaccount:argobot-my-cluster \
  --permission EditChildren:serviceaccount:argobot-my-cluster

# 3. The Channel. --print-subscriptions needs the 0.2 binary; run it from the
#    image. If the 0.1 argobot had CONFIGHUB_EVENT_TARGET_ID set, scope the
#    subscriptions to that Target as described under Setup.
docker run --rm ghcr.io/confighub/argobot:latest --print-subscriptions \
  | cub channel create argobot --space my-cluster \
      --subscriptions - --permission Claim:serviceaccount:argobot-my-cluster
```

4. **Argo CD's pull credentials.** If Argo CD's repo-creds Secret for the ConfigHub registry carries the worker ID and secret, set its `username` to `token` and its `password` to the token from step 1. Argo CD reads the Secret at its next pull, and the token is already valid, so no pull fails. Under Flux, do the same to the pull Secret each OCIRepository's `secretRef` names.
5. **argobot's Secret.** Add `CONFIGHUB_AUTH_PRIVATE_KEY`, the contents of the `.jwk` file from step 1, to `argobot-secrets`. The 0.1 argobot ignores it.
6. **The rollout.** Move the Deployment to the 0.2 image, set `CONFIGHUB_CHANNEL=my-cluster/argobot`, and drop the worker and subscription variables (left in place, they only warn). A one-replica Deployment keeps the 0.1 pod until the 0.2 pod is Ready, so a missing key or Claim shows up as a crash-looping new pod beside a working old one, and reverting the Deployment undoes it. `cub channel get argobot --space my-cluster` shows when the 0.2 argobot holds the Channel.
7. **The worker.** Once the Channel is held, delete the worker with `cub worker delete`. This is the one step the rest cannot undo.

A kind cluster from `cub cluster up` of an earlier cub is simpler to recreate than to upgrade: `cub cluster down --name <name> --delete-config`, then `cub cluster up --name <name>` with the new cub, which creates the service account and the Channel itself. `--delete-config` also deletes the deployment Spaces bound to that cluster.

## Build and release

```sh
go build ./...        # compile
docker build -t ghcr.io/confighub/argobot:dev .
```

Three workflows:

- `.github/workflows/build.yml` builds and pushes **development** images to `ghcr.io/confighub/argobot` on pushes to `main` and on PRs (tags `main`, `pr-N`, `sha-…`). No semver, no `:latest`.
- `.github/workflows/release.yml` cuts a **release** when you push a `vX.Y.Z` git tag: it builds the semver image (`:vX.Y.Z`, plus `:latest`), with the manifests in it, then publishes the installer package to `oci://ghcr.io/confighub/packages/argobot:X.Y.Z`, signed. The manifests and the package name the release they run, so set it first: `scripts/set-version.sh vX.Y.Z`, merge that, then push the tag. `release.yml` fails for a tag they don't name. A manifest or package change ships with the next release.
- `.github/workflows/package.yml` tests the **installer package** in `manifests/package` with `scripts/test-package.sh`, which you can run locally.

Every image carries its third-party notices at `/app/THIRD_PARTY_LICENSES.txt` (Go modules linked into the binary) and `/app/OS_LICENSE_NOTICE.txt` (Alpine base packages, with the GPL source offer), and every published manifest has an SPDX SBOM attestation (`docker buildx imagetools inspect ghcr.io/confighub/argobot:<tag> --format '{{json .SBOM}}'`). After changing dependencies, run `./scripts/gen-third-party-licenses.sh` and commit the result; `.github/workflows/third-party-licenses.yml` fails the PR if the committed file is stale or a copyleft dependency is linked in.
