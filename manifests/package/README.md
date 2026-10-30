# argobot installer package

An [installer](https://github.com/confighub/installer) package for argobot, published as `oci://ghcr.io/confighub/packages/argobot:<version>`.

The base holds what argobot needs under every GitOps controller:
- its Namespace (Pod Security Standards restricted)
- its ServiceAccount
- cluster-wide read-only access, without Secrets
- the Deployment, at a pinned image tag, reading its key from the `argobot-secrets` Secret, which is not part of the package
- argobot's environment, but for its key: the `argobot-env` ConfigMap, generated from `bases/default/argobot.env`

Select one controller component with it:

| Component | Adds |
|---|---|
| `argocd` (default) | A Role and RoleBinding in `argocd`, read access to `argoproj.io`, its variables, `GITOPS_CONTROLLER=argocd` among them (`argocd.env`) |
| `flux` | A ClusterRole to patch OCIRepositories, read access to the Flux source and kustomize groups, its variables, `GITOPS_CONTROLLER=flux` among them (`flux.env`) |
| `sveltos` | A ClusterRole to patch profiles, read access to the Sveltos groups, the HealthCheck and ClusterHealthCheck that live status reads, its variables, `GITOPS_CONTROLLER=sveltos` among them (`sveltos.env`) |

They form the required `controller` group: selecting two fails, and selecting none installs `argocd`. A render fails validation if:
- the image is `:latest` or untagged
- the pod doesn't meet the Pod Security Standards restricted level
- argobot's environment has a variable argobot doesn't read, or a value it doesn't accept, by the schema in `validation/env.schema.json`

### argobot's environment

`argobot-env` is an AppConfig/Env file carried in a ConfigMap: each component's `.env` file merges into the base's. `installer upload` makes it an AppConfig Unit, which ConfigHub renders into the ConfigMap, so you change argobot's environment by editing that Unit, for example:

```sh
cub function do --space argobot-my-cluster --unit argobot-env --toolchain AppConfig/Env \
  set-string-path Argobot ARGO_REFRESH_TYPE normal
```

The ConfigMap has no name suffix (`disableNameSuffixHash`), so ConfigHub renders it in place under the name the Deployment reads. It also fills in the Deployment's `confighub.com/Hash` annotation with the ConfigMap's hash, so a change rolls the pods.

`validation/env.schema.json` is what `argobot docgen env` prints; `scripts/test-package.sh` fails when it differs.

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

Every input is optional, so one install can be the base that each cluster's variant is cloned from. Leave `channel` and `target` unset, and set them in each variant's `argobot-env` Unit:

```sh
installer setup --pull oci://ghcr.io/confighub/packages/argobot:0.2.5 \
  --work-dir argobot-base --namespace argobot
installer upload --work-dir argobot-base --space argobot-base
cub variant create --target my-cluster/target my-cluster argobot-base
cub function do --space argobot-my-cluster --unit argobot-env --toolchain AppConfig/Env \
  set-string-path Argobot CONFIGHUB_CHANNEL my-cluster/argobot
cub function do --space argobot-my-cluster --unit argobot-env --toolchain AppConfig/Env \
  set-string-path Argobot CONFIGHUB_TARGET my-cluster/target
```

`cub cluster attach --k8sbot-installer` installs argobot this way.

## Testing

`scripts/test-package.sh` renders the package for each controller and preset with `installer setup`, running its validators, and checks the renders it has to refuse. It needs installer 0.3.0 or later (or `$INSTALLER`), kustomize and yq, and no ConfigHub server. The `package` workflow runs it on every change.

## Publishing

The package's version is the argobot release it runs: package 0.2.5 runs argobot v0.2.5. `scripts/set-version.sh vX.Y.Z` sets both before a release is tagged, and the release publishes the package after its image (`release.yml`). A version is published once, so a change to the package alone ships with the next argobot release.
