#!/usr/bin/env bash
# Copyright (C) ConfigHub, Inc.
# SPDX-License-Identifier: MIT
#
# No personal names in committed files.
#
# Prose in this repository says "the team", "a colleague" or the issue number
# instead of naming a person; a home directory path is written $HOME and a
# sample identity user@example.com. The tree is clean today. This check keeps
# it clean: it fails when a tracked file contains one of the first names below
# as a whole word, in any letter case, and prints the lines.
#
#   - git grep -w matches a whole word, so a longer identifier that merely
#     contains a name (a GitHub handle, say) is not a hit.
#   - This file holds the pattern, so it is the one file left out of the scan.
#     Keep names out of its prose too.
#
# Run from anywhere in the checkout:
#   ./scripts/verify-no-personal-names.sh
# CI runs it on every push to main and every pull request
# (.github/workflows/build.yml).

set -uo pipefail

PATTERN="alexis|jesper|brian|charlie"
SELF="scripts/verify-no-personal-names.sh"

cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 2

hits="$(git grep -nwiE "$PATTERN" -- . ":(exclude)$SELF")"
status=$?

# git grep exits 1 when nothing matches.
if [[ $status -eq 1 ]]; then
  echo "no-personal-names: clean (every tracked file)"
  exit 0
fi

if [[ $status -eq 0 ]]; then
  echo "$hits" >&2
  echo >&2
  echo "no-personal-names: $(wc -l <<<"$hits" | tr -d " ") line(s) above name a person in a committed file." >&2
  echo "Replace each name with neutral wording (the team / a colleague / the issue number)." >&2
  exit 1
fi

echo "no-personal-names: git grep exited with status $status" >&2
exit 2
