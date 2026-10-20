#!/usr/bin/env bash
# Scenario 8 (issue #38): a full `down` → `up --clone` cycle on the same repo,
# as after a fresh clone of the gitops repo on a new machine.
#   1. Registers a remote-backed app whose origin is a real (local bare) repo,
#      and proves it serves.
#   2. `flywheel down --yes` deletes the cluster.
#   3. The app worktree is deleted (as on a fresh machine).
#   4. `flywheel up --clone` recreates the cluster, clones the worktree back
#      from its recorded origin, and the app builds and serves again.
#
# TERMINAL: it recreates the cluster, so it runs LAST (see run-all.sh), after
# scenario 5 removed sample-app. Uses its own app so it doesn't depend on that.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
APP=clone-app
source ./lib.sh

log "scenario 8: down → up --clone"

cluster="${KCTX#k3d-}"
remote="$WORKSPACES_ROOT/.remotes/$APP.git"
rm -rf "$remote"
# HEAD must name the branch we push (main): a bare repo defaulting to master
# would clone "successfully" with nothing checked out.
git init -q --bare --initial-branch=main "$remote"

# --- 1. a remote-backed app that serves ------------------------------------------
switch_gitops_branch main 2>/dev/null || true
create_sibling_app_repo
git_in "$WORKSPACES_ROOT/$APP" remote set-url origin "$remote"
add_sample_app
grep -qF "url: $remote" "$CLIENT_REPO/flywheel.yaml" ||
  { log "FAIL: add app did not record $APP's origin ($remote)"; exit 1; }
edit_app_and_commit main "hello from $APP before down"
git_in "$WORKSPACES_ROOT/$APP" push -q origin main
wait_for_served_text "hello from $APP before down" "$(scaled 480)"

# --- 2. down -------------------------------------------------------------------
( cd "$CLIENT_REPO" && flywheel down --yes )
if k3d cluster list --no-headers 2>/dev/null | awk '{print $1}' | grep -qx "$cluster"; then
  log "FAIL: cluster $cluster still exists after flywheel down"
  exit 1
fi
log "flywheel down deleted cluster $cluster"

# --- 3. lose the worktree ------------------------------------------------------
rm -rf "${WORKSPACES_ROOT:?}/$APP"
log "deleted worktree $WORKSPACES_ROOT/$APP"

# --- 4. up --clone -------------------------------------------------------------
( cd "$CLIENT_REPO" && flywheel up --clone )
got=$(git -C "$WORKSPACES_ROOT/$APP" log -1 --format=%s 2>/dev/null) ||
  { log "FAIL: up --clone did not recreate $WORKSPACES_ROOT/$APP"; exit 1; }
[[ "$got" == "set text: hello from $APP before down" ]] ||
  { log "FAIL: cloned worktree HEAD is '$got', want the pushed commit"; exit 1; }
log "up --clone restored $APP from $remote"

wait_for_served_text "hello from $APP before down" "$(scaled 480)"

# The loop keeps working on the recreated cluster.
edit_app_and_commit main "hello from $APP after up"
wait_for_served_text "hello from $APP after up" "$(scaled 150)"

log "scenario 8 PASS"
