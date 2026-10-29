# argobot installer package

An [installer](https://github.com/confighub/installer) package for argobot, published as `oci://ghcr.io/confighub/packages/argobot:<version>`.

The base holds what argobot needs under every GitOps controller:
- its Namespace (Pod Security Standards restricted)
- its ServiceAccount
- cluster-wide read-only access, without Secrets
- the `argobot-secrets` Secret
- the Deployment, at a pinned image tag
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
- a service account
- its grants on the cluster's Target
- a Channel that the service account can claim

It needs installer 0.2.0 or later. Unless you pass `private_key_file`, the installer's collector issues the service account's key with `cub`, so `cub` must be signed in to the organization that holds them.

```sh
installer setup --pull oci://ghcr.io/confighub/packages/argobot:0.2.3 \
  --work-dir argobot-my-cluster --namespace argobot \
  --input service_account=argobot-my-cluster \
  --input channel=my-cluster/argobot \
  --input target=my-cluster/target
# The key never goes to ConfigHub: apply its Secret to the cluster yourself,
# with the Namespace it goes in.
kubectl apply -f argobot-my-cluster/out/manifests/namespace-argobot.yaml \
  -f argobot-my-cluster/out/secrets/
installer upload --work-dir argobot-my-cluster --space argobot-base
# Deploy it to the cluster. On a cluster `cub cluster up` or `cub cluster
# attach` set up, this also adds argobot's Argo CD Application.
cub variant create --target my-cluster/target my-cluster argobot-base
cub release publish argobot-my-cluster
```

Add `--select flux` or `--select sveltos` for those controllers.

| Input | Default | Description |
|---|---|---|
| `service_account` | required | The service account argobot authenticates as. |
| `channel` | required | The Channel argobot claims, as `<space>/<slug>`. |
| `target` | none | The Target that stands for the cluster, as `<space>/<slug>`. When set, argobot stores the cluster's facts on it. |
| `private_key_file` | none | A private JWK the service account already has registered, as an absolute path, such as the one `cub serviceaccount key add --generate` writes. Without it, the collector issues a key. |
| `confighub_url` | the cub context's server | The ConfigHub URL as the cluster reaches it. |

Without `private_key_file`, the collector issues one key per work-dir and keeps it in `out/secrets/argobot-private-key.jwk`, so later setups, including upgrades with `setup --pull`, reuse it. `setup --clean` removes it, and the next setup issues a new key. Delete the old one with `cub serviceaccount key delete`.

To run another image, such as a mirror's copy, pass `--set-image ghcr.io/confighub/argobot=<registry>/argobot:<tag>`.

## Testing

`scripts/test-package.sh` renders the package for each controller and preset with `installer setup`, running its validators, and checks the renders it has to refuse. It needs installer 0.2.0 or later (or `$INSTALLER`), kustomize and yq, and no ConfigHub server. The `package` workflow runs it on every change.

## Publishing

The package's version is the argobot release it runs: package 0.2.3 runs argobot v0.2.3. Each argobot release opens a pull request that pins the package to the new image and version (`scripts/pin-package-image.sh`, run by `release.yml`), and merging it publishes the package (`package.yml`). A version is published once, so a change to the package alone ships with the next argobot release; tag one to ship it sooner.
