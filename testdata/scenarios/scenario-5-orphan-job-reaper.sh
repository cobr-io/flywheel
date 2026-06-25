#!/usr/bin/env bash
# Scenario 5: orphan-Job reaper. Removing a per-app builder folder
# (builders/base/<APP>/) leaves no live GitRepository, so Flux can't
# garbage-collect the Kaniko build Jobs in flywheel-system (Kubernetes
# does not honour cross-namespace ownerReferences). image-builder-
# controller's Reconcile-on-delete watches the GitRepository in `apps`,
# sees IsNotFound, and sweeps Jobs labelled app=image-builder,repo=<APP>
# in its own namespace.
#
# Runs after scenarios 1-4: assumes the $APP builder already exists and
# at least one Kaniko Job has run for it.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
source ./lib.sh

log "scenario 5: orphan-Job reaper"

# Pre-condition: the dev loop ran at least once, so a build Job
# labelled repo=$APP exists in flywheel-system.
job_count_before=$(kc -n flywheel-system get jobs -l app=image-builder,repo="$APP" --no-headers 2>/dev/null | wc -l | tr -d ' ')
if [[ "$job_count_before" -lt 1 ]]; then
  log "ERROR: expected at least one build Job for $APP before reap test; run scenario 1 first"
  exit 1
fi
log "found $job_count_before build Job(s) for $APP in flywheel-system"

# Switch back to main on both worktrees so this scenario starts from a
# known state (scenarios 2-4 leave branches checked out).
switch_app_branch main 2>/dev/null || true
switch_gitops_branch main 2>/dev/null || true

# Remove the builder folder + the app workload, rewrite the parent
# kustomizations, and commit. git-auto-sync-self mirrors the commit
# into myapp.git; Flux prunes the GitRepository, ConfigMap, IR/IP, and
# per-app git-auto-sync Deployment.
rm -rf "$CLIENT_REPO/builders/base/$APP" "$CLIENT_REPO/apps/base/$APP"
cat >"$CLIENT_REPO/builders/base/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
EOF
cat >"$CLIENT_REPO/apps/base/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources: []
EOF
git_in "$CLIENT_REPO" add -A
git_in "$CLIENT_REPO" commit -q -m "remove $APP builder + workload (reaper test)"
log "committed removal of $APP builder + workload"

# Wait for Flux to prune the GitRepository in the apps namespace.
log "waiting up to 90s for GitRepository apps/$APP to be pruned"
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  if ! kc -n apps get gitrepository "$APP" >/dev/null 2>&1; then
    log "GitRepository apps/$APP pruned"
    break
  fi
  sleep 2
done
if kc -n apps get gitrepository "$APP" >/dev/null 2>&1; then
  log "TIMEOUT: GitRepository apps/$APP not pruned"
  kc -n flux-system get kustomization client-builders \
    -o jsonpath='{.status.conditions[*].message}' 2>&1 || true
  dump_diag
  exit 1
fi

# Now the controller's Reconcile fires with IsNotFound and reaps every
# Job labelled app=image-builder,repo=$APP. Allow 30s; that's well
# above any reasonable Reconcile + delete latency.
log "waiting up to 30s for build Jobs to be reaped"
deadline=$((SECONDS + 30))
while (( SECONDS < deadline )); do
  remaining=$(kc -n flywheel-system get jobs -l app=image-builder,repo="$APP" --no-headers 2>/dev/null | wc -l | tr -d ' ')
  if [[ "$remaining" == "0" ]]; then
    log "scenario 5 PASS: all build Jobs for $APP reaped"
    exit 0
  fi
  sleep 2
done

log "TIMEOUT: build Jobs for $APP still present after GitRepository was pruned"
kc -n flywheel-system get jobs -l app=image-builder,repo="$APP" 2>&1
log "----- DIAG: image-builder-controller log (tail) -----"
kc -n flywheel-system logs deploy/image-builder-controller --tail=30 2>&1 || true
exit 1
