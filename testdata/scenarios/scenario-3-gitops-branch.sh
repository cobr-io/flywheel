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
# tracks the default branch. Select it explicitly with `flywheel use`.
edit_gitops_replicas_and_commit experiment/raise-replicas 3
flywheel_use experiment/raise-replicas
wait_for_gitrepo_branch flux-system flux-system experiment/raise-replicas 60
wait_for_replicas 3 120

# Regression guard (issue #6 + #17): re-running `flywheel up` must NOT clobber
# the selected branch back to the bootstrap branch. `up` re-applies the
# self-source manifest with the default branch, but the deploy-branch annotation
# is the source of truth and git-auto-sync-self re-asserts it — so `up` can't
# silently move the cluster. The replica bump must survive.
log "scenario 3: re-running 'flywheel up' must preserve the selected branch"
( cd "$CLIENT_REPO" && flywheel up )
wait_for_gitrepo_branch flux-system flux-system experiment/raise-replicas 60
wait_for_replicas 3 120

# Defense-in-depth: even a direct external clobber of spec.ref.branch (no
# `flywheel up`, no checkout) must self-heal from the deploy-branch annotation
# within a couple of poll intervals.
log "scenario 3: external clobber of the gitops source must self-heal"
clobber_gitrepo_branch flux-system flux-system main
wait_for_gitrepo_branch flux-system flux-system experiment/raise-replicas 30

# Graceful degradation (issue #17): deleting the selected branch from the gitops
# repo degrades the deployment back to the default branch (always exists),
# rather than leaving Flux stuck on a vanished ref. Replicas return to the main
# value (1). Check out main first — you can't delete the checked-out branch.
log "scenario 3: deleting the selected branch degrades to the default"
switch_gitops_branch main
delete_gitops_branch experiment/raise-replicas
wait_for_gitrepo_branch flux-system flux-system main 60
wait_for_replicas 1 120

log "scenario 3 PASS"
