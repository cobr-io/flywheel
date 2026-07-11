#!/usr/bin/env bash
# Scenario 2 (plan § 1.4 / T1.8): app-repo branch switch.
# git checkout -b feat/foo in the app repo, commit. The app's
# GitRepository.spec.ref.branch should track feat/foo; the cluster
# reconciles to the feature-branch commit; switch back to main and it
# reconciles back.
#
# Assumes scenario-1 already ran (sample-app added + initial build done).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
source ./lib.sh

log "scenario 2: app-repo branch switch"

# Commit on a feature branch.
edit_app_and_commit feat/foo "hello from feat/foo"
# The per-app GitRepository (git-auto-sync patches it) lives in
# flywheel-system, not apps — apps holds only the workload.
wait_for_gitrepo_branch flywheel-system "$APP" feat/foo "$(scaled 60)"
wait_for_served_text "hello from feat/foo" "$(scaled 180)"

# Switch back to main; should reconcile back to main's content.
switch_app_branch main
wait_for_gitrepo_branch flywheel-system "$APP" main "$(scaled 60)"
# main's last content (from scenario 1 steady-state) is "v2".
wait_for_served_text "hello from sample-app v2" "$(scaled 180)"

log "scenario 2 PASS"
