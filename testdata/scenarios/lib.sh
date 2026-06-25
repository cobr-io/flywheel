#!/usr/bin/env bash
# Shared helpers for the Phase 1 dev-loop validation scenarios (plan § 1.4
# / T1.6-T1.9). Sourced by scenario-*.sh.
#
# These scripts assume a client repo created by `flywheel init` and a
# cluster brought up by `flywheel up`. add_sample_app() invokes
# `flywheel add app` for the per-app builder folder and only scaffolds
# the app workload (apps/base/<name>/) by hand — the per-app-template
# already honours cfg.flywheel.images for the git-auto-sync ref.
set -euo pipefail

: "${KCTX:?set KCTX to the kube context, e.g. k3d-acme-local}"
: "${CLIENT_REPO:?set CLIENT_REPO to the client gitops repo dir}"
: "${WORKSPACES_ROOT:?set WORKSPACES_ROOT (parent of CLIENT_REPO + sibling app repos)}"
: "${REGISTRY:?set REGISTRY, e.g. acme-local-registry}"
: "${REGISTRY_PORT:?set REGISTRY_PORT, e.g. 50001}"
: "${CLIENT_NAME:?set CLIENT_NAME, e.g. acme}"

APP="${APP:-sample-app}"
TESTDATA="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

log() { echo "[$(date '+%H:%M:%S')] $*"; }

kc() { kubectl --context="$KCTX" "$@"; }

# git_in <dir> <args...> — run git in a worktree with a deterministic
# identity and no host hooks.
git_in() {
  local dir="$1"; shift
  git -C "$dir" -c user.email=scenario@flywheel.test -c user.name=scenario \
      -c core.hooksPath=/dev/null "$@"
}

# create_sibling_app_repo — creates the sibling app worktree at
# $WORKSPACES_ROOT/$APP with the sample-app Dockerfile + index.html and an
# initial commit on main. This is the repo git-auto-sync mirrors and
# image-builder-controller builds.
create_sibling_app_repo() {
  local dir="$WORKSPACES_ROOT/$APP"
  rm -rf "$dir"; mkdir -p "$dir"
  cp "$TESTDATA/sample-app/Dockerfile" "$dir/Dockerfile"
  # Distinct initial content so the scenarios' first edit always changes
  # the file (a no-op commit would fail under set -e).
  echo "init" >"$dir/index.html"
  git_in "$dir" init -q --initial-branch=main
  git_in "$dir" add -A
  git_in "$dir" commit -q -m "init sample-app"
  # Give the app an `origin` remote so `flywheel add app` registers it as
  # remote-backed. Without one it is "local-only", and the local-only guard
  # refuses registration on the integration branch (main). add-app only reads
  # the URL (git remote get-url origin), so a placeholder is enough — the
  # in-cluster dev loop syncs via the git-server bare repo, not origin.
  git_in "$dir" remote add origin "https://git.example.test/${CLIENT_NAME}/${APP}.git"
  log "created sibling app repo at $dir"
}

# add_sample_app — scaffolds the per-app builder (via `flywheel add app`)
# and the app workload (by hand, since v0.1 doesn't scaffold those),
# then commits.
add_sample_app() {
  local registry_url="k3d-${REGISTRY}:5000"
  ( cd "$CLIENT_REPO" && flywheel add app "$APP" )

  # App workload.
  local adir="$CLIENT_REPO/apps/base/$APP"
  mkdir -p "$adir"
  sed "s|k3d-acme-local-registry:5000/acme/sample-app|$registry_url/$CLIENT_NAME/$APP|; s|flux-system:sample-app|flux-system:$APP|" \
    "$TESTDATA/sample-app/deployment.yaml" >"$adir/deployment.yaml"
  cat >"$adir/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ./deployment.yaml
EOF
  cat >"$CLIENT_REPO/apps/base/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ./$APP
EOF

  git_in "$CLIENT_REPO" add -A
  git_in "$CLIENT_REPO" commit -q -m "add $APP builder + workload"
  log "added $APP builder + workload to client repo"
}

# wait_for_pod_image <namespace> <label> <expected-substring> <timeout-s>
# Polls until a pod matching <label> is Running with an image tag whose
# rolled-out content matches. We assert on the served HTML instead of the
# tag (more robust): wait_for_served_text.
wait_for_served_text() {
  local want="$1" timeout="${2:-90}"
  local deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    local pod
    pod=$(kc -n apps get pods -l app="$APP" \
      --field-selector=status.phase=Running \
      -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    if [[ -n "$pod" ]]; then
      local got
      got=$(kc -n apps exec "$pod" -- cat /www/index.html 2>/dev/null || true)
      if [[ "$got" == *"$want"* ]]; then
        log "served text matches: $want"
        return 0
      fi
    fi
    sleep 3
  done
  log "TIMEOUT waiting for served text: $want"
  dump_diag
  return 1
}

# dump_diag prints the dev-loop state at a failure point so CI logs reveal
# *where* the chain is stuck (Kustomizations / sources / dev-loop pods /
# build jobs / git-auto-sync) instead of just "no resources".
dump_diag() {
  echo "----- DIAG: Flux Kustomizations -----"
  kc -n flux-system get kustomization 2>&1 || true
  echo "----- DIAG: GitRepositories -----"
  kc get gitrepository -A 2>&1 || true
  echo "----- DIAG: flywheel-system pods -----"
  kc -n flywheel-system get pods 2>&1 || true
  echo "----- DIAG: apps pods + builders -----"
  kc -n apps get pods,gitrepository,cm 2>&1 || true
  echo "----- DIAG: build jobs -----"
  kc -n flywheel-system get jobs 2>&1 || true
  echo "----- DIAG: git-server log (tail) -----"
  kc -n flywheel-system logs deploy/git-server --tail=25 2>&1 || true
  echo "----- DIAG: git-server /workspaces + /srv/git -----"
  kc -n flywheel-system exec deploy/git-server -- sh -c 'ls -la /workspaces; echo ---bare---; ls -la /srv/git' 2>&1 || true
  echo "----- DIAG: git-auto-sync-self log (tail) -----"
  kc -n flywheel-system logs deploy/git-auto-sync-self --tail=20 2>&1 || true
  echo "----- DIAG: flywheel-dev-loop status -----"
  kc -n flux-system get kustomization flywheel-dev-loop -o jsonpath='{.status.conditions[*].message}' 2>&1 || true
  echo
  echo "----- DIAG: client-builders status -----"
  kc -n flux-system get kustomization client-builders -o jsonpath='{.status.conditions[*].message}' 2>&1 || true
  echo
}

# edit_app_and_commit <branch> <new-text> — on the sibling app repo,
# checkout <branch>, change index.html to <new-text>, commit.
edit_app_and_commit() {
  local branch="$1" text="$2"
  local dir="$WORKSPACES_ROOT/$APP"
  if [[ "$branch" != "main" ]]; then
    git_in "$dir" checkout -q -B "$branch"
  else
    git_in "$dir" checkout -q main
  fi
  echo "$text" >"$dir/index.html"
  git_in "$dir" add -A
  git_in "$dir" commit -q -m "set text: $text"
  log "committed '$text' on app branch $branch"
}

# switch_app_branch <branch> — checkout an existing branch in the app
# worktree without committing (used to switch back).
switch_app_branch() {
  git_in "$WORKSPACES_ROOT/$APP" checkout -q "$1"
  log "app worktree switched to branch $1"
}

# wait_for_gitrepo_branch <namespace> <name> <branch> <timeout-s> —
# asserts git-auto-sync patched a Flux GitRepository's spec.ref.branch.
wait_for_gitrepo_branch() {
  local ns="$1" name="$2" want="$3" timeout="${4:-60}"
  local deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    local got
    got=$(kc -n "$ns" get gitrepository "$name" -o jsonpath='{.spec.ref.branch}' 2>/dev/null || true)
    if [[ "$got" == "$want" ]]; then
      log "GitRepository $ns/$name tracks branch $want"
      return 0
    fi
    sleep 2
  done
  log "TIMEOUT: GitRepository $ns/$name never tracked branch $want (last: ${got:-<none>})"
  dump_diag
  return 1
}

# clobber_gitrepo_branch <namespace> <name> <branch> — directly repoint a Flux
# GitRepository's spec.ref.branch, simulating an external actor (e.g. a
# `flywheel up` bootstrap re-apply) that resets the source WITHOUT a worktree
# branch switch. git-auto-sync's drift-correction should then heal it back.
clobber_gitrepo_branch() {
  local ns="$1" name="$2" branch="$3"
  kc -n "$ns" patch gitrepository "$name" --type=merge \
    -p "{\"spec\":{\"ref\":{\"branch\":\"$branch\"}}}" >/dev/null
  log "clobbered GitRepository $ns/$name to branch $branch (simulating external re-apply)"
}

# edit_gitops_replicas_and_commit <branch> <count> — on the client gitops
# worktree, checkout <branch>, set the sample-app Deployment replicas to
# <count>, commit. Drives scenario 3 (client-gitops branch switch).
edit_gitops_replicas_and_commit() {
  local branch="$1" count="$2"
  local dep="$CLIENT_REPO/apps/base/$APP/deployment.yaml"
  if [[ "$branch" != "main" ]]; then
    git_in "$CLIENT_REPO" checkout -q -B "$branch"
  else
    git_in "$CLIENT_REPO" checkout -q main
  fi
  # Replace the first `replicas: N` line.
  sed -i.bak "s/^  replicas: .*/  replicas: $count/" "$dep" && rm -f "$dep.bak"
  git_in "$CLIENT_REPO" add -A
  git_in "$CLIENT_REPO" commit -q -m "set $APP replicas: $count"
  log "committed replicas=$count on gitops branch $branch"
}

# switch_gitops_branch <branch> — checkout an existing branch in the
# client gitops worktree (used to switch back).
switch_gitops_branch() {
  git_in "$CLIENT_REPO" checkout -q "$1"
  log "gitops worktree switched to branch $1"
}

# flywheel_use <branch> — opt-in deploy selection for the gitops repo (issue
# #17). The gitops/self sync no longer auto-follows checkouts; this is how a
# branch becomes the deployed one.
flywheel_use() {
  ( cd "$CLIENT_REPO" && flywheel use "$1" )
  log "flywheel use $1"
}

# delete_gitops_branch <branch> — delete a local branch in the gitops worktree
# (must not be the checked-out branch). Used to test graceful degradation: the
# self sync detects the selected branch vanished and falls back to the default.
delete_gitops_branch() {
  git_in "$CLIENT_REPO" branch -D "$1"
  log "deleted gitops branch $1"
}

# wait_for_replicas <count> <timeout-s> — asserts the sample-app
# Deployment reaches the given desired replica count.
wait_for_replicas() {
  local want="$1" timeout="${2:-90}"
  local deadline=$((SECONDS + timeout))
  while (( SECONDS < deadline )); do
    local got
    got=$(kc -n apps get deploy "$APP" -o jsonpath='{.spec.replicas}' 2>/dev/null || true)
    if [[ "$got" == "$want" ]]; then
      log "Deployment $APP replicas=$want"
      return 0
    fi
    sleep 2
  done
  log "TIMEOUT: Deployment $APP never reached replicas=$want (last: ${got:-<none>})"
  dump_diag
  return 1
}
