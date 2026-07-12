#!/usr/bin/env bash
# T27 — completion-path drift check.
#
# scripts/install-completions.sh is the single source of truth for the three
# shell-completion destination paths (zsh/bash/fish). install.sh inlines its
# own copy (it's fetched standalone via curl, before a repo checkout exists,
# so it can't source install-completions.sh) and uninstall.sh keeps a static
# fallback copy for the same standalone case. Both copies are hand-maintained
# and can silently drift from the source of truth.
#
# This script extracts each file's path list and fails if any of the three
# disagree. Run it locally exactly as CI does:
#   bash scripts/check-completion-paths-drift.sh
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

# ---- 1. source of truth: scripts/install-completions.sh -------------------
truth="$(bash scripts/install-completions.sh --print-paths | sort)"

# ---- 2. uninstall.sh's static fallback list --------------------------------
# Lines look like:  remove_completion zsh  "${HOME}/.zsh/completions/_flywheel"
uninstall_paths="$(
  grep -E '^[[:space:]]*remove_completion[[:space:]]+(zsh|bash|fish)[[:space:]]' uninstall.sh |
  while read -r _ shell rest; do
    # shellcheck disable=SC2154  # path is assigned by the eval above it
    ( eval "path=$rest"; printf '%s %s\n' "$shell" "$path" )
  done | sort
)"

# ---- 3. install.sh's inlined case block ------------------------------------
# Lines look like:  zsh)  dir="${HOME}/.zsh/completions"; dest="${dir}/_flywheel" ;;
binary_line="$(grep -m1 '^BINARY=' install.sh)"
eval "$binary_line"   # sets $BINARY, referenced by install.sh's bash/fish dest exprs

install_paths="$(
  grep -E '^[[:space:]]*(zsh|bash|fish)\)[[:space:]]+dir=' install.sh |
  while read -r rawline; do
    shell="$(sed -E 's/^[[:space:]]*([a-z]+)\).*/\1/' <<<"$rawline")"
    body="$(sed -E 's/^[[:space:]]*[a-z]+\)[[:space:]]*//; s/[[:space:]]*;;[[:space:]]*$//' <<<"$rawline")"
    # shellcheck disable=SC2154  # dest is assigned by the eval above it
    ( eval "$body"; printf '%s %s\n' "$shell" "$dest" )
  done | sort
)"

fail=0
if [ "$truth" != "$uninstall_paths" ]; then
  echo "DRIFT: uninstall.sh completion paths != scripts/install-completions.sh --print-paths" >&2
  diff <(echo "$truth") <(echo "$uninstall_paths") >&2 || true
  fail=1
fi
if [ "$truth" != "$install_paths" ]; then
  echo "DRIFT: install.sh completion paths != scripts/install-completions.sh --print-paths" >&2
  diff <(echo "$truth") <(echo "$install_paths") >&2 || true
  fail=1
fi

if [ "$fail" -eq 0 ]; then
  echo "ok: completion paths agree across install.sh, uninstall.sh, and scripts/install-completions.sh"
fi
exit "$fail"
