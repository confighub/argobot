#!/usr/bin/env bash
# Copyright (C) ConfigHub, Inc.
# SPDX-License-Identifier: MIT
#
# Pins the installer package in manifests/package to an argobot release: the
# image tag in the base's kustomize images block and its Deployment, and the
# package's version, which is the release's without the "v". Does nothing when
# the package already runs that release. release.yml runs it for each release.
#
# Needs yq, diff and patch.
#
# Usage: scripts/pin-package-image.sh vX.Y.Z

set -euo pipefail

tag="${1:-}"
[[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "usage: $0 vX.Y.Z" >&2; exit 2; }

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
pkg="${repo}/manifests/package"
export IMAGE=ghcr.io/confighub/argobot TAG="$tag" VERSION="${tag#v}"

# yq_edit <file> <expression>: applies a yq expression in place, keeping the
# file's blank lines, which yq drops: only yq's other changes are patched in.
yq_edit() {
  local file=$1 expression=$2 changes
  changes="$(yq "$expression" "$file" | diff -B "$file" - || true)"
  [ -z "$changes" ] || printf '%s\n' "$changes" | patch -s "$file"
}

kustomization="${pkg}/bases/default/kustomization.yaml"
current="$(yq -r '.images[] | select(.name == strenv(IMAGE)) | .newTag' "$kustomization")"
if [ "$current" = "$tag" ]; then
  echo "The package already runs ${tag}."
  exit 0
fi

yq_edit "$kustomization" '(.images[] | select(.name == strenv(IMAGE))).newTag = strenv(TAG)'
yq_edit "${pkg}/bases/default/deployment.yaml" \
  '(.spec.template.spec.containers[] | select(.name == "argobot")).image = strenv(IMAGE) + ":" + strenv(TAG)'
yq_edit "${pkg}/installer.yaml" '.installerMetadata.version = strenv(VERSION)'

echo "Pinned the package to ${IMAGE}:${tag}, as version $(yq -r .installerMetadata.version "${pkg}/installer.yaml")."
