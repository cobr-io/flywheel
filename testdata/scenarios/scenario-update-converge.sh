#!/usr/bin/env bash
# Scenario: `flywheel update` converges an already-up cluster idempotently.
# Regression for the update command (Phase 3-4 / the issue #9 delivery path):
# a dry-run plans without mutating; a real --cluster-only converge re-imports
# images + re-applies machinery and leaves git-server healthy; a second run is
# a no-op now that the cluster baseline is recorded.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
source ./lib.sh

log "scenario update: cluster convergence"

cd "$CLIENT_REPO"

# Dry-run: prints the Layer-A plan and mutates nothing.
flywheel update --cluster-only --dry-run

# Real converge against the live cluster (images already in host docker /
# registry from `up`). Must succeed and leave git-server rolled out.
flywheel update --cluster-only --yes
kubectl --context "$KCTX" -n flywheel-system rollout status deploy/git-server --timeout=120s

# Idempotent: the cluster baseline is now recorded, so a second run finds no
# drift and converges to a no-op without error.
flywheel update --cluster-only --yes

log "scenario update PASS"
