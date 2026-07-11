#!/usr/bin/env bash
# Scenario dispatcher — the single entry point shared by the reusable CI
# recipe (.github/workflows/e2e-recipe.yml) and the local driver
# (scripts/e2e.sh). It selects which of the in-cluster dev-loop scenarios to
# run against an already-up cluster, so per-PR CI, the nightly full run, and a
# local `scripts/e2e.sh` all drive the SAME scenario scripts through one place.
#
# Usage:
#   run.sh all       # every scenario, in order, via run-all.sh (nightly)
#   run.sh "1 5"     # a space-separated subset (the per-PR CI gate)
#
# The scenarios are ORDER-COUPLED by design (see the header of run-all.sh):
# scenario-2 asserts scenario-1's leftover state and scenario-5 hard-exits
# without a build Job that an earlier scenario produced. A subset therefore
# has to remain a valid chain — the two shapes in use are "all" (nightly) and
# "1 5" (per-PR: baseline + orphan-Job reaper; 5 only needs 1's prior build
# Job, not 2-4). Do NOT invent arbitrary subsets without checking the chain.
#
# Required env (see lib.sh): KCTX, CLIENT_REPO, WORKSPACES_ROOT, CLIENT_NAME.
# TIMEOUT_SCALE (optional, default 1) scales every wait ceiling — CI exports 3.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

sel="${1:-all}"

if [ "$sel" = "all" ]; then
  exec bash ./run-all.sh
fi

read -r -a scenarios <<<"$sel"
for n in "${scenarios[@]}"; do
  matches=(./scenario-"$n"-*.sh)
  if [ ! -e "${matches[0]}" ]; then
    echo "run.sh: no scenario script matches 'scenario-$n-*.sh'" >&2
    exit 1
  fi
  echo "==================== scenario $n (${matches[0]}) ===================="
  bash "${matches[0]}"
done
echo "==================== SCENARIOS ($sel) PASS ===================="
