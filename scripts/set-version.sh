#!/usr/bin/env bash
# Copyright (C) ConfigHub, Inc.
# SPDX-License-Identifier: MIT
#
# Sets the argobot release the deployments name, before that release is tagged:
# the image tag in manifests/argobot.yaml, manifests/flux/argobot.yaml and
# manifests/sveltos/argobot.yaml, which the release's image carries, and in the
# installer package in manifests/package, whose version is the release's without
# the "v". Commit the change and merge it, then push the tag. release.yml fails
# for a tag the deployments don't name.
#
# With --check it changes nothing and fails when the deployments name another
# release.
#
# Needs yq, diff, patch and perl.
#
# Usage: scripts/set-version.sh [--check] vX.Y.Z

set -euo pipefail

check=false
if [ "${1:-}" = "--check" ]; then
  check=true
  shift
fi
tag="${1:-}"
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "usage: $0 [--check] vX.Y.Z" >&2; exit 2; }

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pkg="${repo}/manifests/package"
manifests=("${repo}/manifests/argobot.yaml" "${repo}/manifests/flux/argobot.yaml" "${repo}/manifests/sveltos/argobot.yaml")
export IMAGE=ghcr.io/confighub/argobot TAG="$tag" VERSION="${tag#v}"

if $check; then
  wrong=0
  for f in "${manifests[@]}" "${pkg}/bases/default/deployment.yaml"; do
    grep -q "image: ${IMAGE}:${tag}\$" "$f" || { echo "${f#"$repo"/} does not run ${IMAGE}:${tag}" >&2; wrong=1; }
  done
  [ "$(yq -r '.images[] | select(.name == strenv(IMAGE)) | .newTag' "${pkg}/bases/default/kustomization.yaml")" = "$tag" ] \
    || { echo "manifests/package/bases/default/kustomization.yaml does not pin ${tag}" >&2; wrong=1; }
  [ "$(yq -r .installerMetadata.version "${pkg}/installer.yaml")" = "$VERSION" ] \
    || { echo "manifests/package/installer.yaml is not version ${VERSION}" >&2; wrong=1; }
  [ "$wrong" = 0 ] || { echo "Run scripts/set-version.sh ${tag}, merge it, and tag again." >&2; exit 1; }
  echo "The deployments run ${IMAGE}:${tag}."
  exit 0
fi

# yq_edit <file> <expression>: applies a yq expression in place, keeping the
# file's blank lines, which yq drops: only yq's other changes are patched in.
yq_edit() {
  local file=$1 expression=$2 changes
  changes="$(yq "$expression" "$file" | diff -B "$file" - || true)"
  [ -z "$changes" ] || printf '%s\n' "$changes" | patch -s "$file"
}

for f in "${manifests[@]}"; do
  perl -pi -e 's{^(\s*image: \Q$ENV{IMAGE}\E):v[0-9]+\.[0-9]+\.[0-9]+$}{$1:$ENV{TAG}}' "$f"
done
yq_edit "${pkg}/bases/default/kustomization.yaml" '(.images[] | select(.name == strenv(IMAGE))).newTag = strenv(TAG)'
yq_edit "${pkg}/bases/default/deployment.yaml" \
  '(.spec.template.spec.containers[] | select(.name == "argobot")).image = strenv(IMAGE) + ":" + strenv(TAG)'
yq_edit "${pkg}/installer.yaml" '.installerMetadata.version = strenv(VERSION)'

"$0" --check "$tag"
