#!/usr/bin/env bash
# Runs the six dev-loop validation scenarios in sequence against an
# already-up cluster (flywheel init + flywheel up done). Invoked via run.sh
# ("all") from the reusable CI recipe (nightly) and local scripts/e2e.sh.
#
# ORDER-COUPLED BY DESIGN — the scenarios share state and MUST run in order;
# this is deliberate (making each self-bootstrapping would multiply runtime by
# re-seeding the app + waiting on a cold build every time). The chain:
#   1 baseline      — seeds the sample-app (add app + first + steady build);
#                     leaves main at content "v2", replicas=1.
#   2 app-branch    — asserts scenario-1's app exists; branch-switches it.
#   3 gitops-branch — asserts scenario-1's app exists; selects gitops branches.
#   4 both-branches — both repos on independent feature branches at once;
#                     leaves the app worktree back on main.
#   6 branch-stress — rapid-fire branch-flip race guard for issue #86; needs
#                     the app present on main (scenario-1) and MUST run
#                     BEFORE scenario-5, which deletes the app entirely.
#   5 orphan reaper — hard-exits unless a prior build Job exists (needs 1);
#                     DESTRUCTIVE + TERMINAL: removes the app's builder and
#                     workload, so nothing may run against this app after it.
# NUMBERING NOTE: scenario-6's file is named for the order it was added
# (after 1-5 existed), not the order it runs — it executes as the 5th step,
# right before the (renumbered-in-position-only) scenario-5. Do NOT reorder
# this loop to "1 2 3 4 5 6"; that would run the destructive scenario-5
# before scenario-6 has an app left to stress.
#
# A subset run (run.sh "1 5") must therefore stay a valid chain: 5 depends
# only on 1's build Job, so "1 5" is safe (scenario-6 is skipped in that
# subset, matching per-PR CI — see e2e-recipe.yml); "2" alone is not.
#
# Required env (see lib.sh): KCTX, CLIENT_REPO, WORKSPACES_ROOT, CLIENT_NAME.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

for s in scenario-1-baseline scenario-2-app-branch \
         scenario-3-gitops-branch scenario-4-both-branches \
         scenario-6-branch-stress scenario-5-orphan-job-reaper; do
  echo "==================== $s ===================="
  bash "./$s.sh"
done
echo "==================== ALL SCENARIOS PASS ===================="
