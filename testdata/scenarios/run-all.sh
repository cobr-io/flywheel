#!/usr/bin/env bash
# Runs the four dev-loop validation scenarios in sequence against an
# already-up cluster (flywheel init + flywheel up done). Scenario 1 seeds
# the sample-app; 2-4 exercise branch switches on that state; 5 removes
# the builder and asserts image-builder-controller reaps orphan Jobs.
# Used by CI (1.6) and manual pre-release validation.
#
# Required env (see lib.sh): KCTX, CLIENT_REPO, WORKSPACES_ROOT, REGISTRY,
# REGISTRY_PORT, CLIENT_NAME.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

for s in scenario-1-baseline scenario-2-app-branch \
         scenario-3-gitops-branch scenario-4-both-branches \
         scenario-5-orphan-job-reaper; do
  echo "==================== $s ===================="
  bash "./$s.sh"
done
echo "==================== ALL SCENARIOS PASS ===================="
