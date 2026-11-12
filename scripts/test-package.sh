#!/usr/bin/env bash
# Copyright (C) ConfigHub, Inc.
# SPDX-License-Identifier: MIT
#
# Renders the installer package in manifests/package the way an operator
# would, with `installer setup`, for each controller and preset, and checks the
# renders it has to refuse. The package's validators (vet-schemas, vet-images,
# vet-pod-security-standard, ...) run in every render. It also checks that the
# plain manifests run the same release as the package, with the same
# environment.
#
# Needs installer 0.3.0 or later (or $INSTALLER) and yq on PATH, and no
# ConfigHub server.
#
# Usage: scripts/test-package.sh

set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pkg="${repo}/manifests/package"
installer="${INSTALLER:-installer}"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

common=(--non-interactive --namespace argobot)

failures=0
fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

# The package's version is the argobot release it runs.
version="$(yq -r .installerMetadata.version "${pkg}/installer.yaml")"
tag="$(yq -r '.images[] | select(.name == "ghcr.io/confighub/argobot") | .newTag' "${pkg}/bases/default/kustomization.yaml")"
[ "v${version}" = "$tag" ] || fail "installerMetadata.version is ${version}, but the package runs argobot ${tag}"
"${repo}/scripts/set-version.sh" --check "$tag" >/dev/null || fail "the deployments in manifests/ don't all run argobot ${tag}"

# render <case> <installer setup args...> renders the package.
render() {
  local name=$1
  shift
  "$installer" setup --pull "$pkg" --work-dir "${work}/${name}" "${common[@]}" "$@" \
    > "${work}/${name}.log" 2>&1
}

# env_of <case> <variable>: the variable's value in argobot's environment.
env_of() {
  yq -r ".spec.template.spec.containers[0].env[] | select(.name == \"$2\") | .value" \
    "${work}/$1/out/manifests/deployment-argobot-argobot.yaml"
}

# env_list <file>: the environment of the argobot Deployment in file, sorted
# by name, one variable per line.
env_list() {
  yq -o json -I 0 'select(.kind == "Deployment") | .spec.template.spec.containers[0].env | sort_by(.name) | .[] | sort_keys(..)' "$1"
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
  [ -z "$(ls -A "${out}/secrets" 2>/dev/null)" ] || fail "${name}: rendered a Secret; argobot-secrets is supplied out of band"
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

connected=(--input channel=test/argobot --input target=test/target --input confighub_url=https://hub.example.com)
expect_render base argocd
expect_render default argocd --components default "${connected[@]}"
expect_render minimal argocd --components minimal "${connected[@]}"
expect_render all argocd --components all "${connected[@]}"
expect_render flux flux --select flux "${connected[@]}"
expect_render sveltos sveltos --select sveltos "${connected[@]}"

# The Argo CD Role stays in Argo CD's namespace; argobot's own resources move.
ns="$(yq -r '.metadata.namespace' "${work}/default/out/manifests/role-argocd-argobot-argocd.yaml")"
[ "$ns" = argocd ] || fail "default: the Role is in ${ns}, want argocd"
# A base, rendered with no inputs, leaves the Channel and Target for each
# variant to set.
for v in CONFIGHUB_CHANNEL CONFIGHUB_TARGET; do
  [ "$(env_of base "$v")" = confighubplaceholder ] || fail "base: ${v} is $(env_of base "$v"), want confighubplaceholder"
done
[ "$(env_of base CONFIGHUB_URL)" = https://hub.confighub.com ] || fail "base: CONFIGHUB_URL is $(env_of base CONFIGHUB_URL)"
for v in CONFIGHUB_CHANNEL:test/argobot CONFIGHUB_TARGET:test/target CONFIGHUB_URL:https://hub.example.com; do
  [ "$(env_of flux "${v%%:*}")" = "${v#*:}" ] || fail "flux: ${v%%:*} is $(env_of flux "${v%%:*}"), want ${v#*:}"
done
# Rendered with no inputs, as a base, each controller's Deployment has the
# environment of its plain manifest.
expect_render flux-base flux --select flux
expect_render sveltos-base sveltos --select sveltos
for case in base:argobot.yaml flux-base:flux/argobot.yaml sveltos-base:sveltos/argobot.yaml; do
  if ! diff -u <(env_list "${repo}/manifests/${case#*:}") \
    <(env_list "${work}/${case%%:*}/out/manifests/deployment-argobot-argobot.yaml") >&2; then
    fail "${case%%:*}: the environment differs from manifests/${case#*:}"
  fi
done
# Metrics are on unless the metrics input turns them off.
[ "$(env_of base METRICS_ENABLED)" = true ] || fail "base: METRICS_ENABLED is $(env_of base METRICS_ENABLED), want true"
expect_render no-metrics argocd --input metrics=false
[ "$(env_of no-metrics METRICS_ENABLED)" = false ] || fail "no-metrics: METRICS_ENABLED is $(env_of no-metrics METRICS_ENABLED), want false"
ports="$(yq -o json -I 0 '.spec.template.spec.containers[0].ports' "${work}/no-metrics/out/manifests/deployment-argobot-argobot.yaml")"
[ "$ports" = null ] || fail "no-metrics: ports are ${ports}, want none"
ports="$(yq -r '.spec.template.spec.containers[0].ports[].name' "${work}/base/out/manifests/deployment-argobot-argobot.yaml")"
[ "$ports" = metrics ] || fail "base: ports are ${ports:-none}, want metrics"
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
