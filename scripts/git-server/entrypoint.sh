#!/bin/bash
# Serves bare git repositories over HTTP with both fetch and push enabled.
#
# Differences from a stock git-server setup:
#   - http.receivepack=true is set per repo (anonymous push, cluster-internal
#     service only, never exposed via Ingress).
#   - GIT_HTTP_RECEIVE_PACK=1 is exported into the fcgiwrap env so the
#     git-http-backend serves smart-HTTP receive-pack.
#   - Bare repos live on a PVC at /srv/git. We do NOT clone from the worktree
#     at startup if a bare repo already exists; the bare repo is the
#     source-of-truth for any commits the cluster (IAC) has made that the
#     worktree hasn't pulled yet.

set -euo pipefail

export HOME=/tmp

# Trust every worktree under /workspaces regardless of ownership. The
# host-mounted worktrees are owned by the developer/CI uid (e.g. 1001),
# not the git-server container's uid, so git's "dubious ownership" guard
# would otherwise abort `git clone --bare <worktree>` and the bare repo
# would never be created (symptom: Flux self-source "repository not
# found"). A blanket safe.directory is correct here: this is a
# single-tenant local dev git-server, not a shared host.
git config --global --add safe.directory '*'

mkdir -p /srv/git

# A process killed mid-write (e.g. SIGKILL during a Deployment rollout, which
# must release the RWO PVC) can leave stale git *.lock files on the persisted
# PVC. Those would make every subsequent `git config`/ref write fail, and under
# `set -e` that aborts the entrypoint — crash-looping the server forever with no
# self-heal. Nothing is writing these repos yet (this runs before the background
# rescan and before fcgiwrap), so it's safe to clear all of them here.
find /srv/git -name '*.lock' -delete 2>/dev/null || true

# enable_push <bare-dir> — configure a bare repo to accept anonymous
# in-cluster HTTP pushes (git-auto-sync, IAC).
enable_push() {
    local bare="$1"
    # Defense-in-depth: never let one repo's config error abort the whole
    # entrypoint under `set -e`. A failing repo is logged and skipped so the
    # server still comes up and serves the others.
    git -C "$bare" config http.receivepack true || { echo "WARN: enable_push $bare failed (http.receivepack)"; return 0; }
    # IAC clones, commits, then pushes — bare repos have no worktree so
    # denyCurrentBranch isn't relevant; set ignore as belt-and-suspenders.
    git -C "$bare" config receive.denyCurrentBranch ignore || { echo "WARN: enable_push $bare failed (denyCurrentBranch)"; return 0; }
    touch "$bare/git-daemon-export-ok"
    chmod -R a+rx "$bare"
}

# scan_workspaces — create a bare repo for every /workspaces/<name>/ that
# has a .git and doesn't already have one. Idempotent. Run once at startup
# AND periodically in the background, so apps added to a running cluster
# (`flywheel add-app` / scenario tests) get a bare repo without a
# git-server restart.
scan_workspaces() {
    for repo_dir in "${WORKSPACES_DIR}"/*; do
        [ -d "$repo_dir/.git" ] || continue
        git config --global --add safe.directory "$repo_dir" 2>/dev/null || true
        local name bare
        name=$(basename "$repo_dir")
        bare="/srv/git/${name}.git"
        if [ ! -d "$bare" ]; then
            echo "Creating bare repo $bare from $repo_dir"
            if ! git clone --bare "$repo_dir" "$bare" 2>&1; then
                echo "WARN: failed to clone $repo_dir → $bare (will retry next scan)"
                rm -rf "$bare"
                continue
            fi
        fi
        enable_push "$bare"
    done
}

scan_workspaces

# Pre-create an empty bare repo for the Flywheel mirror. `flywheel up`
# step 11c pushes the cached Flywheel clone here over smart-HTTP;
# git-http-backend doesn't auto-create repos, so the push target must
# exist first. The repo stays empty until that push lands. The
# FLYWHEEL_MIRROR_REPO env var names it (default: flywheel.git).
MIRROR_REPO="${FLYWHEEL_MIRROR_REPO:-flywheel}"
MIRROR_BARE="/srv/git/${MIRROR_REPO}.git"
if [ ! -d "$MIRROR_BARE" ]; then
    echo "Pre-creating empty Flywheel mirror bare repo $MIRROR_BARE"
    git init --bare "$MIRROR_BARE"
fi
# `flywheel up` step 11c pushes the cache to refs/heads/main. Point the
# bare repo's HEAD at main so Flux (which clones the default branch to
# resolve a commit pin) finds it — otherwise HEAD defaults to the git
# version's initial branch (master) and Flux reports "reference not found".
git -C "$MIRROR_BARE" symbolic-ref HEAD refs/heads/main
enable_push "$MIRROR_BARE"

# Background rescan loop: pick up app repos added after startup.
( while true; do sleep "${WORKSPACES_RESCAN_INTERVAL:-5}"; scan_workspaces || true; done ) &

ln -sf /dev/stdout /var/log/nginx/access.log
ln -sf /dev/stderr /var/log/nginx/error.log

cat >/etc/nginx/nginx.conf <<'EOF'
events {}
http {
  client_max_body_size 100m;
  server {
    listen 8080;
    server_name localhost;

    location ~ (/.*\.git/.*) {
      include fastcgi_params;
      fastcgi_param SCRIPT_FILENAME /usr/lib/git-core/git-http-backend;
      fastcgi_param GIT_PROJECT_ROOT /srv/git;
      fastcgi_param GIT_HTTP_EXPORT_ALL 1;
      fastcgi_param GIT_HTTP_RECEIVE_PACK 1;
      fastcgi_param PATH_INFO $1;
      fastcgi_pass 127.0.0.1:9000;
    }
  }
}
EOF

# spawn-fcgi inherits GIT_HTTP_RECEIVE_PACK from this shell.
spawn-fcgi -p 9000 /usr/sbin/fcgiwrap &

exec nginx -g 'daemon off;'
