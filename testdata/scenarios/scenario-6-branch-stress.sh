#!/usr/bin/env bash
# Scenario 6 (design docs/designs/2026-07-17-per-app-sync-controller-design.md
# "Test plan"; issue #86): rapid-fire app-branch-flip stress test.
#
# The old bash sidecar's race: a `git checkout` landing between its
# "snapshot HEAD" and "reset --hard" steps could reset against the WRONG
# branch, or leave the in-cluster bare repo's main pointed at a commit from
# an already-abandoned branch ("poison" — ImagePolicy then latches the
# highest-timestamp tag built from that poisoned content). internal/appsync
# closes the race by taking every decision against a `for-each-ref` snapshot
# (never bare HEAD) and re-verifying (post-verify) after any mutating step,
# rolling back and aborting the tick if a checkout raced it. This scenario
# deliberately hammers that window: ~10 sub-second checkouts between
# feat/stress and main with NO waits in between, so several flips land
# inside the controller's poll/tick window.
#
# ORDER-COUPLING (see run-all.sh's chain comment — READ IT before moving
# this): this MUST run after scenarios 1-4 (it needs the app scenario-1
# seeded, present on main) and BEFORE the destructive scenario 5, which
# deletes the app's builder + workload entirely — there would be nothing
# left to stress afterward. run-all.sh therefore sequences the chain
# 1, 2, 3, 4, 6, 5 — scenario-6 is NOT simply appended after 5 despite its
# numeric name.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
source ./lib.sh

log "scenario 6: rapid branch-flip stress (issue #86 regression guard)"

# Precondition: the app worktree exists (scenario-1 already ran).
if [[ ! -d "$WORKSPACES_ROOT/$APP/.git" ]]; then
  log "ERROR: expected $WORKSPACES_ROOT/$APP to be a git worktree; run scenario 1 first"
  exit 1
fi

# scenario-4 leaves the app worktree checked out on main, but switch
# defensively in case this is invoked standalone or a prior scenario left it
# elsewhere.
switch_app_branch main 2>/dev/null || true
wait_for_gitrepo_branch flywheel-system "$APP" main "$(scaled 60)"

# Establish a KNOWN final main content BEFORE the stress loop, so the
# post-stress served-text assertion has an unambiguous target — main's
# content otherwise drifts across the scenario chain (scenario-1/2/4 all
# leave different text on it).
edit_app_and_commit main "stress-baseline"
wait_for_served_text "stress-baseline" "$(scaled 360)"

# Create the flip partner: a real branch with DISTINCT content (not just a
# ref pointing at the same tree — a poisoned bare main would then be
# undetectable by content or by sha).
edit_app_and_commit feat/stress "stress-feat-content"
# edit_app_and_commit left the worktree on feat/stress; return to main
# before the stress loop below.
switch_app_branch main

# --- the stress loop --------------------------------------------------------
# ~10 rapid flips, sub-second gaps, NO waits — the whole point is landing
# checkouts inside the controller's tick window, not letting it settle
# between flips. Direct `git_in checkout` (skip switch_app_branch's log
# line) to keep every flip as fast as possible. Always end on main.
log "starting rapid-fire flip loop (10 iterations, no waits, no sleeps)"
for _ in $(seq 1 10); do
  git_in "$WORKSPACES_ROOT/$APP" checkout -q feat/stress
  git_in "$WORKSPACES_ROOT/$APP" checkout -q main
done
log "flip loop done; worktree left on main"

# --- settle -------------------------------------------------------------
wait_for_gitrepo_branch flywheel-system "$APP" main "$(scaled 60)"
wait_for_served_text "stress-baseline" "$(scaled 360)"

# --- assertion (a): served text converged to main's (known) content --------
# (proven by wait_for_served_text above; logged explicitly so all three
# Test-plan assertions have a visible PASS line.)
log "assertion (a) PASS: served text converged to main's content (stress-baseline)"

# --- assertion (b): NO POISON — bare main sha == worktree main sha ---------
bare=$(kc -n flywheel-system exec deploy/git-server -- sh -c \
  "git -C /srv/git/${APP}.git rev-parse refs/heads/main" | tr -d '\r\n')
work=$(git_in "$WORKSPACES_ROOT/$APP" rev-parse refs/heads/main)
if [[ "$bare" != "$work" ]]; then
  log "FAIL: bare main sha ($bare) != worktree main sha ($work) — the bare repo was POISONED by a raced checkout (the exact #86 failure)"
  dump_diag
  exit 1
fi
log "assertion (b) PASS: no poison — bare main sha == worktree main sha ($bare)"

# --- assertion (c): index.html still writable by the runner user -----------
# The old sidecar ran as root; its `reset --hard` could leave root-owned
# files behind, breaking the next `git commit` under the host uid (EACCES).
# Check the mode bit AND actually perform a write (a stale root-owned file
# can still show a misleading mode bit under some overlay/bind-mount setups).
idx="$WORKSPACES_ROOT/$APP/index.html"
if [[ ! -w "$idx" ]]; then
  log "FAIL: index.html not writable by runner — root-owned file regression"
  dump_diag
  exit 1
fi
# shellcheck disable=SC2005 # deliberate: `cat "$idx" >"$idx"` would truncate
# the file before cat opens it; command substitution reads the full content
# in a subshell BEFORE the outer redirect truncates+rewrites.
if ! echo "$(cat "$idx")" >"$idx" 2>/dev/null; then
  log "FAIL: index.html not writable by runner — root-owned file regression (write attempt failed despite -w)"
  dump_diag
  exit 1
fi
log "assertion (c) PASS: index.html writable by runner uid (mode check + actual write succeeded)"

log "scenario 6 PASS"
