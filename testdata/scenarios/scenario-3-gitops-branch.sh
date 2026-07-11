#!/usr/bin/env bash
# Scenario 3 (plan § 1.4 / T1.8; opt-in rework for issue #17): client-gitops
# branch selection. The gitops/self sync no longer auto-follows worktree
# checkouts — auto-following is dangerous on the repo that carries infra (a
# transient checkout, e.g. `git rebase`, would deploy + prune an infra-less
# tip). The deployed branch is chosen explicitly with `flywheel use`, recorded
# in a durable deploy-branch annotation that git-auto-sync-self reconciles
# spec.ref.branch to. This scenario exercises: explicit selection, the
# `flywheel up` / external-clobber drift-correction (issue #6 + #17), and
# graceful degradation when the selected branch is deleted.
#
# Assumes scenario-1 already ran. The self git-auto-sync drives the
# gitops-repo sync; `flywheel use` drives the deploy selection.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
source ./lib.sh

log "scenario 3: client-gitops branch selection (opt-in)"

# Raise replicas on a feature branch and commit. In opt-in mode the gitops sync
# mirrors the branch to the bare repo but does NOT auto-deploy it — Flux still
# deploys the constant DEPLOY branch. Select the feature branch explicitly with
# `flywheel use`; git-deploy-controller then feeds it into DEPLOY.
edit_gitops_replicas_and_commit experiment/raise-replicas 3
flywheel_use experiment/raise-replicas
wait_for_deploy_branch experiment/raise-replicas "$(scaled 60)"
# Deploy-ref isolation: `use` records the selection in an annotation and does
# NOT repoint the self GitRepository's spec.ref.branch (stays flywheel/local-deploy).
assert_self_gitrepo_on_deploy_ref
wait_for_replicas 3 "$(scaled 120)"

# Regression guard (issue #6 + #17): re-running `flywheel up` must NOT clobber
# the selection back to the default branch. `up` re-applies the self-source
# manifest, but `flywheel use` writes the deploy-branch annotation under a
# distinct SSA field manager, so it survives `up`'s apply and git-deploy-controller
# keeps feeding the selected branch into DEPLOY. The replica bump must survive.
log "scenario 3: re-running 'flywheel up' must preserve the selected branch"
( cd "$CLIENT_REPO" && flywheel up )
wait_for_deploy_branch experiment/raise-replicas "$(scaled 60)"
wait_for_replicas 3 "$(scaled 120)"

# Graceful degradation (issue #17): deleting the selected branch from the gitops
# repo degrades the deployment back to the default branch — git-deploy-controller
# falls back when the selected AUTHORED branch no longer exists in the worktree,
# rather than leaving DEPLOY stuck on a vanished ref. Replicas return to the main
# value (1). Check out main first — you can't delete the checked-out branch.
log "scenario 3: deleting the selected branch degrades to the default"
switch_gitops_branch main
delete_gitops_branch experiment/raise-replicas
wait_for_replicas 1 "$(scaled 120)"

log "scenario 3 PASS"
