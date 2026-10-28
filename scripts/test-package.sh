#!/usr/bin/env bash
# Copyright (C) ConfigHub, Inc.
# SPDX-License-Identifier: MIT
#
# Renders the installer package in manifests/package the way an operator
# would, with `installer setup`, for each controller and preset, and checks the
# renders it has to refuse. The package's validators (vet-schemas, vet-images,
# vet-pod-security-standard, ...) run in every render.
#
# Needs installer 0.2.0 or later (or $INSTALLER), kustomize and yq on PATH. It
# gives the collector a key and a URL, so it runs no cub and needs no ConfigHub
# server.
#
# Usage: scripts/test-package.sh

set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pkg="${repo}/manifests/package"
installer="${INSTALLER:-installer}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# The collector copies it into the Secret; nothing authenticates with it.
printf '{"kty":"OKP","crv":"Ed25519","x":"x","d":"d"}\n' > "${work}/key.jwk"
common=(
  --non-interactive --namespace argobot
  --input service_account=test
  --input channel=test/argobot
  --input "private_key_file=${work}/key.jwk"
  --input confighub_url=https://hub.example.com
)

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

render() {
  local name=$1
  shift
  "$installer" setup --pull "$pkg" --work-dir "${work}/${name}" "${common[@]}" "$@" \
    > "${work}/${name}.log" 2>&1
}

# env_of <case> <variable>: the variable's value on argobot's container.
env_of() {
  yq -r ".spec.template.spec.containers[] | select(.name == \"argobot\") | .env[] | select(.name == \"$2\") | .value" \
    "${work}/$1/out/manifests/deployment-argobot-argobot.yaml"
}

# expect_render <case> <controller> <installer setup args...>: the render
# succeeds, selects exactly that controller's component, and configures argobot
# for it.
expect_render() {
  local name=$1 controller=$2
  shift 2
  if ! render "$name" "$@"; then
    fail "${name}: render failed"
    sed 's/^/  /' "${work}/${name}.log" >&2
    return
  fi
  local out="${work}/${name}/out"
  local components
  components="$(yq -r '.spec.components | join(",")' "${out}/record/selection.yaml")"
  [ "$components" = "$controller" ] || fail "${name}: selected ${components}, want ${controller}"
  local got
  got="$(env_of "$name" GITOPS_CONTROLLER)"
  [ "$got" = "$controller" ] || fail "${name}: GITOPS_CONTROLLER=${got}, want ${controller}"
  [ -s "${out}/secrets/secret-argobot-argobot-secrets.yaml" ] || fail "${name}: no argobot-secrets Secret"
  echo "ok: ${name}"
}

# expect_refusal <case> <message> <installer setup args...>: the render fails,
# saying message.
expect_refusal() {
  local name=$1 want=$2
  shift 2
  if render "$name" "$@"; then
    fail "${name}: rendered, want a refusal saying: ${want}"
    return
  fi
  if ! grep -qF -- "$want" "${work}/${name}.log"; then
    fail "${name}: refused without saying: ${want}"
    sed 's/^/  /' "${work}/${name}.log" >&2
    return
  fi
  echo "ok: ${name}"
}

expect_render default argocd --components default
expect_render minimal argocd --components minimal
expect_render all argocd --components all
expect_render flux flux --select flux --input target=test/target
expect_render sveltos sveltos --select sveltos

# The Argo CD Role stays in Argo CD's namespace; argobot's own resources move.
ns="$(yq -r '.metadata.namespace' "${work}/default/out/manifests/role-argocd-argobot-argocd.yaml")"
[ "$ns" = argocd ] || fail "default: the Role is in ${ns}, want argocd"
[ -z "$(env_of default CONFIGHUB_TARGET)" ] || fail "default: CONFIGHUB_TARGET is set without the target input"
[ "$(env_of flux CONFIGHUB_TARGET)" = test/target ] || fail "flux: CONFIGHUB_TARGET is not the target input"
# The HealthCheck and ClusterHealthCheck are cluster-scoped.
for f in healthcheck-argobot-live-status.yaml clusterhealthcheck-argobot-live-status.yaml; do
  [ -s "${work}/sveltos/out/manifests/${f}" ] || fail "sveltos: ${f} was not rendered"
done

expect_refusal two-controllers 'are alternatives in group "controller"' --select argocd --select flux
expect_refusal latest-image vet-images \
  --set-image ghcr.io/confighub/argobot=ghcr.io/confighub/argobot:latest

if [ "$failures" -gt 0 ]; then
  echo "${failures} check(s) failed" >&2
  exit 1
fi
echo "All package checks passed."
