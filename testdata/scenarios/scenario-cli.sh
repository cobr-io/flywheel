#!/usr/bin/env bash
# Cluster-free CLI coverage (issue #38): the error paths and repo-only commands
# no in-cluster scenario touches. Like scenario-doctor.sh it needs NO cluster,
# so CI runs it right after `flywheel init` and BEFORE the expensive `up`; it
# is deliberately NOT part of run-all.sh. It works on throwaway copies of the
# init'd client repo and never modifies CLIENT_REPO itself. It asserts:
#   1. INVALID CONFIG: `flywheel up` on an invalid flywheel.yaml, an invalid
#      flywheel.yaml.local, and unparseable YAML exits non-zero, names the
#      problem, and creates no cluster (load-config runs before any host or
#      cluster work).
#   2. LOCAL-ONLY GUARD: `flywheel add app` refuses to register an app with no
#      `origin` remote on the integration branch.
#   3. PUBLISH-APP: on a feature branch the local-only app registers; then
#      `publish-app` refuses without an origin and with an unpushed branch,
#      succeeds once pushed (flywheel.yaml local_only → url), and is idempotent.
#
# Required env: CLIENT_REPO (a repo freshly created by `flywheel init`).
set -euo pipefail

: "${CLIENT_REPO:?set CLIENT_REPO to a repo created by flywheel init}"
log() { echo "[$(date '+%H:%M:%S')] $*"; }
git_in() {
  local dir="$1"; shift
  git -C "$dir" -c user.email=scenario@flywheel.test -c user.name=scenario \
      -c core.hooksPath=/dev/null "$@"
}
# A throwaway workspaces root under $HOME, not mktemp's default: macOS temp dirs
# can't be bind-mounted, and add app / publish-app resolve worktrees as siblings
# of the client repo (workspaces_root defaults to the repo's parent).
work="$(mktemp -d "${HOME:-.}/.flywheel-cli-scenario.XXXXXX")"
trap 'rm -rf "$work"' EXIT

cluster=$(awk '/^[[:space:]]*name:/ && c {gsub(/[" ]/,"",$2); print $2; exit} /^cluster:/{c=1}' "$CLIENT_REPO/flywheel.yaml")
[[ -n "$cluster" ]] || { log "FAIL: could not read cluster.name from $CLIENT_REPO/flywheel.yaml"; exit 1; }

# copy_repo <name> — a fresh copy of the init'd client repo at $work/<name>.
copy_repo() {
  rm -rf "${work:?}/$1"
  cp -R "$CLIENT_REPO" "$work/$1"
  rm -f "$work/$1/flywheel.yaml.local"
  echo "$work/$1"
}

# expect_up_fails <repo> <want-substring> <label> — `up` must exit non-zero,
# print <want-substring>, and leave no cluster behind.
expect_up_fails() {
  local repo="$1" want="$2" label="$3" out="$work/up.txt"
  if (cd "$repo" && flywheel up --no-clone) >"$out" 2>&1; then
    log "FAIL ($label): flywheel up exited 0"
    cat "$out"; exit 1
  fi
  if ! grep -qF -- "$want" "$out"; then
    log "FAIL ($label): flywheel up output does not mention '$want'"
    cat "$out"; exit 1
  fi
  if k3d cluster list --no-headers 2>/dev/null | awk '{print $1}' | grep -qx "$cluster"; then
    log "FAIL ($label): flywheel up created cluster $cluster despite the invalid config"
    exit 1
  fi
  log "$label: up fails fast ('$want'), no cluster created"
}

log "scenario cli: invalid config, local-only guard, publish-app"

# --- 1. invalid config --------------------------------------------------------
repo=$(copy_repo bad-schema)
sed -i.bak 's/^\(  memory_limit:\) 256Mi/\1 lots/' "$repo/flywheel.yaml" && rm -f "$repo/flywheel.yaml.bak"
grep -q 'memory_limit: lots' "$repo/flywheel.yaml" || { log "FAIL: could not plant an invalid git_auto_sync.memory_limit"; exit 1; }
expect_up_fails "$repo" "is not a valid memory quantity" "invalid flywheel.yaml"

repo=$(copy_repo bad-local)
printf 'paths:\n  workspaces_root: relative/dir\n' >"$repo/flywheel.yaml.local"
expect_up_fails "$repo" "must be an absolute path" "invalid flywheel.yaml.local"

repo=$(copy_repo bad-yaml)
printf 'cluster: [unclosed\n' >>"$repo/flywheel.yaml"
expect_up_fails "$repo" "parse flywheel.yaml" "unparseable flywheel.yaml"

# --- 2. local-only guard ------------------------------------------------------
repo=$(copy_repo gitops)
app=localapp
app_dir="$work/$app"
mkdir -p "$app_dir"
echo "local" >"$app_dir/index.html"
printf 'FROM busybox\nCOPY index.html /www/index.html\n' >"$app_dir/Dockerfile"
git_in "$app_dir" init -q --initial-branch=main
git_in "$app_dir" add -A
git_in "$app_dir" commit -q -m "init $app"

git_in "$repo" checkout -q main
if (cd "$repo" && flywheel add app "$app") >"$work/guard.txt" 2>&1; then
  log "FAIL: add app registered a local-only app on the integration branch"
  cat "$work/guard.txt"; exit 1
fi
grep -qF "refuse to register a local-only app" "$work/guard.txt" ||
  { log "FAIL: add app refused, but not with the local-only guard"; cat "$work/guard.txt"; exit 1; }
log "local-only guard: add app refuses on main"

# --- 3. publish-app -----------------------------------------------------------
git_in "$repo" checkout -q -b feature/localapp
(cd "$repo" && flywheel add app "$app") >"$work/add.txt" 2>&1 ||
  { log "FAIL: add app on a feature branch"; cat "$work/add.txt"; exit 1; }
grep -qE '^[[:space:]]*local_only: true' "$repo/flywheel.yaml" ||
  { log "FAIL: add app did not record $app as local_only"; cat "$repo/flywheel.yaml"; exit 1; }
log "add app on a feature branch: $app recorded local_only"

# expect_publish_fails <want-substring> <label>
expect_publish_fails() {
  if (cd "$repo" && flywheel publish-app "$app") >"$work/pub.txt" 2>&1; then
    log "FAIL ($2): publish-app exited 0"; cat "$work/pub.txt"; exit 1
  fi
  grep -qF -- "$1" "$work/pub.txt" ||
    { log "FAIL ($2): publish-app output does not mention '$1'"; cat "$work/pub.txt"; exit 1; }
  log "publish-app $2: refused ('$1')"
}
expect_publish_fails "has no 'origin' remote" "without origin"

remote="$work/remotes/$app.git"
git init -q --bare "$remote"
git_in "$app_dir" remote add origin "$remote"
expect_publish_fails "isn't pushed to origin" "with unpushed branch"

git_in "$app_dir" push -q origin main
(cd "$repo" && flywheel publish-app "$app") >"$work/pub.txt" 2>&1 ||
  { log "FAIL: publish-app after push"; cat "$work/pub.txt"; exit 1; }
grep -qF "published $app: source is now $remote" "$work/pub.txt" ||
  { log "FAIL: publish-app success message missing"; cat "$work/pub.txt"; exit 1; }
if ! grep -qF "url: $remote" "$repo/flywheel.yaml" || grep -qE '^[[:space:]]*local_only: true' "$repo/flywheel.yaml"; then
  log "FAIL: flywheel.yaml not switched from local_only to url"; cat "$repo/flywheel.yaml"; exit 1
fi
log "publish-app: $app now remote-backed ($remote)"

(cd "$repo" && flywheel publish-app "$app") >"$work/pub.txt" 2>&1 ||
  { log "FAIL: second publish-app exited non-zero"; cat "$work/pub.txt"; exit 1; }
grep -qF "already remote-backed" "$work/pub.txt" ||
  { log "FAIL: second publish-app is not a no-op"; cat "$work/pub.txt"; exit 1; }
log "publish-app is idempotent"

log "scenario cli PASS"
