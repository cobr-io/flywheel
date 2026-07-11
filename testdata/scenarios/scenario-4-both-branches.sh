#!/usr/bin/env bash
# Scenario 4 (plan § 1.4 / T1.8): both repos on independent feature
# branches simultaneously. App on feat/both, gitops on experiment/both;
# both reconcile independently; switch each back independently. Exercises
# the decoupling of the two git-auto-sync instances (per-app vs self).
#
# Uses branch names distinct from scenarios 2/3 so the bare repos don't
# carry divergent history for the same branch name — git-auto-sync uses
# --force-with-lease, which a developer force-recreating an already-pushed
# branch would trip (an unusual action, out of scope for this scenario).
#
# Assumes scenario-1 already ran (leaves main at "v2", replicas=1).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
source ./lib.sh

log "scenario 4: both repos on independent feature branches"

# Put both repos on fresh feature branches. The app repo auto-follows its
# checkout; the gitops repo is opt-in (issue #17), so select it with
# `flywheel use`. This also exercises the decoupling: the app's automatic
# follow and the gitops repo's explicit selection don't interfere.
edit_app_and_commit feat/both "hello from both-branches feat"
edit_gitops_replicas_and_commit experiment/both 2
flywheel_use experiment/both

# Both should reconcile independently. The per-app GitRepository (spec.ref.branch)
# lives in flywheel-system and follows the app branch; the gitops selection is
# recorded in the self GitRepository's deploy-branch annotation (deploy-ref
# isolation — its spec.ref.branch stays flywheel/local-deploy).
wait_for_gitrepo_branch flywheel-system "$APP" feat/both "$(scaled 60)"
wait_for_deploy_branch experiment/both "$(scaled 60)"
# App-content changes travel the full image chain (build → ImagePolicy scan →
# IUA bump → DEPLOY branch → Flux rollout). With the gitops repo concurrently on
# a feature branch, that chain converges in ~40s locally but is slower on a
# constrained CI runner; give it a generous window (see dump_diag's app-image
# chain section if it still times out).
wait_for_served_text "hello from both-branches feat" "$(scaled 360)"
wait_for_replicas 2 "$(scaled 120)"

# Switch the app back to main; the gitops selection is unchanged (a worktree
# checkout on the app side must not affect the gitops deploy).
switch_app_branch main
wait_for_gitrepo_branch flywheel-system "$APP" main "$(scaled 60)"
wait_for_deploy_branch experiment/both "$(scaled 30)"  # unchanged
# Switching the app back rebuilds main's content into a fresh (newest-tag) image
# that the IUA bumps onto DEPLOY; same image chain as above, so the same generous
# window. This is the step that timed out at 180s in CI (converged in ~40s locally).
wait_for_served_text "hello from sample-app v2" "$(scaled 360)"
wait_for_replicas 2 "$(scaled 60)"  # gitops still selected on its feature branch → still 2

# Select gitops back to main too.
flywheel_use main
wait_for_deploy_branch main "$(scaled 60)"
wait_for_replicas 1 "$(scaled 120)"

log "scenario 4 PASS"
