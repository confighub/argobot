# argobot installer package

An [installer](https://github.com/confighub/installer) package for argobot, published as `oci://ghcr.io/confighub/packages/argobot:<version>`.

The base holds what argobot needs under every GitOps controller:
- its Namespace (Pod Security Standards restricted)
- its ServiceAccount
- `list` on CustomResourceDefinitions, StorageClasses and IngressClasses, for cluster facts
- the Deployment, at a pinned image tag, with argobot's environment in its `env`, reading its key from the `argobot-secrets` Secret, which is not part of the package

Select one controller component with it:

| Component | Adds |
|---|---|
| `argocd` (default) | A Role and RoleBinding in `argocd` to read and patch Applications, and its variables: `GITOPS_CONTROLLER=argocd`, `ARGO_NAMESPACE`, `ARGO_REFRESH_TYPE` |
| `flux` | A ClusterRole to read and patch OCIRepositories and read Kustomizations, and `GITOPS_CONTROLLER=flux` |
| `sveltos` | A ClusterRole to read and patch profiles and read ClusterSummaries and HealthCheckReports, the HealthCheck and ClusterHealthCheck that live status reads, and `GITOPS_CONTROLLER=sveltos` |

They form the required `controller` group: selecting two fails, and selecting none installs `argocd`. A render fails validation if:
- the image is `:latest` or untagged
- the pod doesn't meet the Pod Security Standards restricted level

### argobot's environment

argobot's variables are in its Deployment's `env`, as in the plain deployments in [`manifests/`](..): the base sets the ones every controller reads, and each controller component adds its own with a patch. Change one in ConfigHub with `set-env`, for example:

```sh
cub function do --space argobot-my-cluster --unit argobot \
  set-env argobot ARGO_REFRESH_TYPE=normal
```

## Install

argobot needs the following before installing it, set up as under [Setup](../../README.md#setup):
- a service account, and a key of it (`cub serviceaccount key add --generate`)
- its grants on the cluster's Target
- a Channel that the service account can claim

It needs installer 0.3.0 or later. The key never goes to ConfigHub: put it in the `argobot-secrets` Secret in the install namespace yourself, or with your secret store, under the key `CONFIGHUB_AUTH_PRIVATE_KEY`.

```sh
installer setup --pull oci://ghcr.io/confighub/packages/argobot:0.2.5 \
  --work-dir argobot-my-cluster --namespace argobot \
  --input channel=my-cluster/argobot \
  --input target=my-cluster/target
installer upload --work-dir argobot-my-cluster --space argobot-my-cluster
kubectl create namespace argobot
kubectl create secret generic argobot-secrets -n argobot \
  --from-file=CONFIGHUB_AUTH_PRIVATE_KEY=argobot-my-cluster.jwk
```

Add `--select flux` or `--select sveltos` for those controllers.

| Input | Default | Description |
|---|---|---|
| `channel` | `confighubplaceholder` | The Channel argobot claims, as `<space>/<slug>`. |
| `target` | `confighubplaceholder` | The Target that stands for the cluster, as `<space>/<slug>`. argobot stores the cluster's facts on it. |
| `confighub_url` | `https://hub.confighub.com` | The ConfigHub URL as the cluster reaches it. |

To run another image, such as a mirror's copy, pass `--set-image ghcr.io/confighub/argobot=<registry>/argobot:<tag>`.

### As a base for variants

Every input is optional, so one install can be the base that each cluster's variant is cloned from. Leave `channel` and `target` unset, and set them in each variant's `argobot` Unit, which holds the Deployment:

```sh
installer setup --pull oci://ghcr.io/confighub/packages/argobot:0.2.5 \
  --work-dir argobot-base --namespace argobot
installer upload --work-dir argobot-base --space argobot-base
cub variant create --target my-cluster/target my-cluster argobot-base
cub function do --space argobot-my-cluster --unit argobot \
  set-env argobot CONFIGHUB_CHANNEL=my-cluster/argobot CONFIGHUB_TARGET=my-cluster/target
```

`cub cluster attach --k8sbot-installer` installs argobot this way.

## Testing

`scripts/test-package.sh` renders the package for each controller and preset with `installer setup`, running its validators, and checks the renders it has to refuse. It also checks that each controller's Deployment has the environment of its plain deployment in `manifests/`. It needs installer 0.3.0 or later (or `$INSTALLER`) and yq, and no ConfigHub server. The `package` workflow runs it on every change.

## Publishing

The package's version is the argobot release it runs: package 0.2.5 runs argobot v0.2.5. `scripts/set-version.sh vX.Y.Z` sets both before a release is tagged, and the release publishes the package after its image (`release.yml`). A version is published once, so a change to the package alone ships with the next argobot release.
