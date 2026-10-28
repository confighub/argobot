#!/usr/bin/env bash
# Copyright (C) ConfigHub, Inc.
# SPDX-License-Identifier: MIT
#
# argobot's collector. installer setup runs it on every setup, with the package
# working copy as the working directory.
#
# Side effect: writes the service account's private key to
# bases/default/argobot-private-key.env.secret, which the argobot-secrets
# secretGenerator reads. The key is issued once per work-dir and kept in
# out/secrets/argobot-private-key.jwk, which out/.gitignore covers: setup runs
# this again on every re-render, and `setup --pull` replaces the package
# working copy, so a key kept only there would be issued anew each time, and
# every one of them would stay registered.
#
# Stdout: the facts the function chain reads.
#
#   configHubURL: <the confighub_url input, or the cub context's server>

set -euo pipefail

err() { echo "collector: $*" >&2; exit 1; }

command -v cub >/dev/null || err "cub not found on PATH"

service_account="${INSTALLER_INPUT_SERVICE_ACCOUNT:-}"
[ -n "$service_account" ] || err "the service_account input is required"

kept_key="${INSTALLER_OUT_DIR}/secrets/argobot-private-key.jwk"
if [ ! -s "$kept_key" ]; then
  mkdir -p "${INSTALLER_OUT_DIR}/secrets"
  chmod 700 "${INSTALLER_OUT_DIR}/secrets"
  rm -f "$kept_key"
  # cub writes the private key before registering the public half, and
  # removes it if registration fails. Its report goes to stderr: stdout is
  # the facts.
  cub serviceaccount key add "$service_account" \
    --generate "$kept_key" \
    --description "argobot in namespace ${INSTALLER_NAMESPACE}" >&2 \
    || err "could not issue a key for service account ${service_account}"
fi

umask 077
cp "$kept_key" "${INSTALLER_PACKAGE_DIR}/bases/default/argobot-private-key.env.secret"

configHubURL="${INSTALLER_INPUT_CONFIGHUB_URL:-}"
if [ -z "$configHubURL" ]; then
  configHubURL=$(cub context get -o jq=.coordinate.serverURL --quiet)
  [ -n "$configHubURL" ] || err "could not read the server URL from the cub context; set the confighub_url input"
fi

echo "configHubURL: ${configHubURL}"
