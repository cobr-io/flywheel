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

# Both should reconcile independently. The per-app GitRepository lives in
# flywheel-system (apps holds only the workload); the gitops/self source is
# the flux-system GitRepository.
wait_for_gitrepo_branch flywheel-system "$APP" feat/both 60
wait_for_gitrepo_branch flux-system flux-system experiment/both 60
wait_for_served_text "hello from both-branches feat" 180
wait_for_replicas 2 120

# Switch the app back to main; the gitops selection is unchanged (a worktree
# checkout on the app side must not affect the gitops deploy).
switch_app_branch main
wait_for_gitrepo_branch flywheel-system "$APP" main 60
wait_for_gitrepo_branch flux-system flux-system experiment/both 30  # unchanged
wait_for_served_text "hello from sample-app v2" 180
wait_for_replicas 2 60  # gitops still selected on its feature branch → still 2

# Select gitops back to main too.
flywheel_use main
wait_for_gitrepo_branch flux-system flux-system main 60
wait_for_replicas 1 120

log "scenario 4 PASS"
