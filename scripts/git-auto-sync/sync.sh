#!/bin/bash
# gitops/self-sync moved to internal/selfsync (git-deploy-controller); this
# script serves per-app git-auto-sync sidecars only.
#
# Bidirectional sync between a worktree (host-mounted) and a bare repo
# (served by the in-cluster git-server).
#
# Loop:
#   1. Detect current branch in worktree; if it changed, patch the Flux
#      GitRepository to track the new branch — switch branches, see them
#      deploy.
#   2. git fetch the bare repo's view of <branch>.
#   3. git rebase the worktree onto FETCH_HEAD. This pulls in anything the
#      bare repo has that the worktree doesn't — e.g. a teammate or CI
#      pushing straight to the bare repo over HTTP.
#   4. git push the worktree's current branch to the bare repo.
#
# A rebase conflict means the developer touched the same lines as whatever
# updated the bare repo out-of-band. We abort the rebase, log loudly, and
# stall for 30s before retrying — the developer sees the stall in `kubectl
# logs` and resolves it by `git pull --rebase` in their worktree (which also
# clears the cluster-side state, since the next push fast-forwards).
#
# Env (required):
#   WORKTREE                - absolute path to the worktree, inside the container
#   BARE_REPO_URL           - http URL of the bare repo on the in-cluster git-server
#   GITREPOSITORY_NAME      - Flux GitRepository name to patch on branch switch
#   GITREPOSITORY_NAMESPACE - namespace of that GitRepository

set -euo pipefail

: "${WORKTREE:?WORKTREE required}"
: "${BARE_REPO_URL:?BARE_REPO_URL required}"
: "${GITREPOSITORY_NAME:?GITREPOSITORY_NAME required}"
: "${GITREPOSITORY_NAMESPACE:?GITREPOSITORY_NAMESPACE required}"

POLL_INTERVAL="${POLL_INTERVAL:-2}"

# Optional: when the bare repo head changes (we pushed the developer's commit,
# or fetched something pushed straight to the bare repo), also poke Flux to
# reconcile immediately instead of waiting out a poll interval. Annotating the
# source with reconcile.fluxcd.io/requestedAt collapses stacked poll latency
# out of the commit-to-pod loop. Flux treats the annotation value as opaque;
# it only has to change between triggers.
#
# Keep this bash copy in sync with internal/naming (ReconcileRequestAnnotation).
# Agreement between the Go constants and templates/manifests is enforced Go-side
# only (see internal/cli/converge/naming_agreement_test.go); this shell literal
# is not covered by that test, so update both together.
RECONCILE_ANNOTATION="reconcile.fluxcd.io/requestedAt"

git config --global --add safe.directory "$WORKTREE"
git config --global user.email "git-auto-sync@dev.local"
git config --global user.name "git-auto-sync"
# Disable host pre-commit hooks running in the container.
git config --global core.hooksPath /dev/null

# Shared-worktree permissions. The worktree's .git is bind-mounted from the
# host and written by BOTH this container (as root) and the host user (the
# developer, or the CI runner — non-root). Git creates each .git/objects/<xx>
# fan-out directory owned by whoever writes the first object with that prefix;
# on Linux bind mounts, dirs root creates are root-owned and mode 0755, so the
# host user can't add objects there afterward. That surfaces intermittently
# (it depends on which hash prefixes each side touches first) as the host's
# next commit failing with "insufficient permission for adding an object to
# repository database .git/objects". (On macOS/Docker Desktop the userspace
# mount virtualises ownership, so it only bites real Linux bind mounts — i.e.
# CI.) Tell git to create objects, the index, and refs group/other-writable so
# either user can always write, and retro-fix any restrictive object dirs a
# prior root-owned run already left behind. Best-effort: a missing/late
# worktree just retries the operations that need it.
if [ -d "$WORKTREE/.git" ]; then
    git -C "$WORKTREE" config core.sharedRepository 0777 || true
    chmod -R a+rwX "$WORKTREE/.git/objects" 2>/dev/null || true
fi

current_branch() {
    git -C "$WORKTREE" rev-parse --abbrev-ref HEAD 2>/dev/null || true
}

# heal_index_if_corrupt rebuilds a corrupt/unreadable .git/index from HEAD.
#
# The worktree's .git is bind-mounted and written by BOTH this container (root)
# and the host developer (see the core.sharedRepository note above). A
# concurrent or interrupted index write can truncate or garble .git/index; git
# then aborts every index-reading operation with "fatal: index file corrupt".
# Left unhealed that wedges the loop: the diff guard below misreads the error as
# "uncommitted changes" (NOT hard-resetting) and the rebase path misreads it as
# a conflict, so the loop stalls forever on a transient corruption (issue #4).
#
# Rebuilding only rewrites .git/index from the committed tree — working-tree
# file contents are left untouched, so no uncommitted *edits* are lost (the
# staged/unstaged split is reset, which a sync robot doesn't own). Best-effort:
# logs and returns 0 either way so the loop continues and retries.
heal_index_if_corrupt() {
    local err
    # ls-files reads the index and nothing else; capture only stderr so a clean
    # index (exit 0) short-circuits. The `&&` keeps this safe under `set -e`.
    err=$(git -C "$WORKTREE" ls-files 2>&1 >/dev/null) && return 0
    case "$err" in
        *"index file corrupt"* | *"index file smaller than expected"* | *"bad index file"* | *"unknown index"*)
            echo "$(date '+%F %T') - .git/index is corrupt ($err); rebuilding from HEAD (working-tree files preserved)"
            rm -f "$WORKTREE/.git/index"
            if git -C "$WORKTREE" reset -q; then
                echo "$(date '+%F %T') - .git/index rebuilt from HEAD"
            else
                echo "$(date '+%F %T') - warning: index rebuild failed (no commits yet?); will retry next loop"
            fi
            ;;
        *)
            # Some other ls-files failure (e.g. the worktree isn't mounted yet);
            # not corruption — leave it for the existing retry paths.
            : ;;
    esac
    return 0
}

# The branch the Flux GitRepository is currently pointed at. Used for
# drift-correction: if an external actor (e.g. a `flywheel up` bootstrap
# re-apply, or any future re-applier) clobbers spec.ref.branch without a
# worktree branch switch, the switch-detection logic below never fires and
# the clobber would otherwise persist. Empty if the resource is absent.
gitrepository_branch() {
    kubectl get gitrepository "$GITREPOSITORY_NAME" \
        -n "$GITREPOSITORY_NAMESPACE" \
        -o jsonpath='{.spec.ref.branch}' 2>/dev/null || true
}

patch_gitrepository() {
    local new_branch="$1"
    echo "$(date '+%F %T') - patching GitRepository '$GITREPOSITORY_NAME/$GITREPOSITORY_NAMESPACE' to branch '$new_branch'"
    # First, stop Flux's kustomize-controller from re-reconciling this
    # resource. Otherwise it would re-apply the static `branch: main` from
    # the source manifest every interval and race our patch. The annotation
    # can't live in the source manifest (Flux skips creation entirely if it's
    # there), so we add it imperatively. Idempotent.
    if ! kubectl annotate gitrepository "$GITREPOSITORY_NAME" \
        -n "$GITREPOSITORY_NAMESPACE" \
        'kustomize.toolkit.fluxcd.io/reconcile=disabled' --overwrite >/dev/null 2>&1; then
        echo "$(date '+%F %T') - warning: GitRepository annotate failed (likely not created yet); will retry next loop"
        return 1
    fi
    if ! kubectl patch gitrepository "$GITREPOSITORY_NAME" \
        -n "$GITREPOSITORY_NAMESPACE" \
        --type=merge \
        -p "{\"spec\":{\"ref\":{\"branch\":\"$new_branch\"}}}" >/dev/null 2>&1; then
        echo "$(date '+%F %T') - warning: GitRepository patch failed; will retry next loop"
        return 1
    fi
    return 0
}

trigger_reconcile() {
    # Poke Flux to reconcile now; called after the bare repo head changes.
    # Best-effort: a failed annotate just falls back to the normal poll
    # interval, so warn but never abort the sync loop.
    local now
    now="$(date -u '+%FT%TZ')"
    if ! kubectl annotate gitrepository "$GITREPOSITORY_NAME" \
        -n "$GITREPOSITORY_NAMESPACE" \
        "${RECONCILE_ANNOTATION}=${now}" --overwrite >/dev/null 2>&1; then
        echo "$(date '+%F %T') - warning: reconcile trigger on GitRepository '$GITREPOSITORY_NAME' failed (will rely on poll)"
    fi
}

LAST_BRANCH=""

echo "$(date '+%F %T') - starting bidirectional sync: $WORKTREE <-> $BARE_REPO_URL"

while true; do
    branch=$(current_branch)

    if [ -z "$branch" ] || [ "$branch" = "HEAD" ]; then
        echo "$(date '+%F %T') - detached or empty branch, skipping"
        sleep "$POLL_INTERVAL"
        continue
    fi

    # Rebuild a corrupt .git/index before any index-reading op below, so a
    # transient corruption self-heals instead of wedging the loop (issue #4).
    heal_index_if_corrupt

    if [ "$branch" != "$LAST_BRANCH" ]; then
        echo "$(date '+%F %T') - branch is '$branch' (was '${LAST_BRANCH:-<none>}')"
        # Only advance LAST_BRANCH if the patch+annotate actually succeeded.
        # This makes the loop self-heal when the GitRepository doesn't exist
        # yet at startup (it'll keep retrying every POLL_INTERVAL until Flux
        # creates the resource), instead of caching a "we already synced this
        # branch" state from a failed attempt.
        if patch_gitrepository "$branch"; then
            LAST_BRANCH="$branch"
        fi
    elif [ -n "$LAST_BRANCH" ]; then
        # Defense-in-depth drift correction. No worktree switch happened
        # (branch == LAST_BRANCH), so the switch path above didn't run — but
        # an external actor may have clobbered spec.ref.branch out from under
        # us (the issue-#6 failure mode: a `flywheel up` bootstrap re-apply
        # repointing the source at a stale branch). Re-assert the branch the
        # developer is actually on so any such clobber self-heals within a
        # poll interval. Cheap in steady state: one `kubectl get`, and a patch
        # only when drift is seen.
        live_branch=$(gitrepository_branch)
        if [ -n "$live_branch" ] && [ "$live_branch" != "$branch" ]; then
            echo "$(date '+%F %T') - drift: GitRepository on '$live_branch' but worktree on '$branch'; re-asserting"
            patch_gitrepository "$branch" || true
        fi
    fi

    # Pull anything the bare repo has that we don't.
    # --no-tags keeps the worktree's tag set clean.
    if ! git -C "$WORKTREE" fetch --no-tags "$BARE_REPO_URL" "$branch" 2>/dev/null; then
        # Branch may not exist in bare yet (first push), that's fine — skip
        # the rebase, push will create it.
        git -C "$WORKTREE" -c core.hooksPath=/dev/null push "$BARE_REPO_URL" "$branch:$branch" 2>/dev/null || true
        sleep "$POLL_INTERVAL"
        continue
    fi

    bare_head=$(git -C "$WORKTREE" rev-parse FETCH_HEAD 2>/dev/null || echo "")
    work_head=$(git -C "$WORKTREE" rev-parse HEAD)
    should_trigger=0

    if [ -n "$bare_head" ] && [ "$bare_head" != "$work_head" ]; then
        # Only attempt rebase when fast-forward isn't possible AND there's
        # something to integrate. Try fast-forward first (no work to do if
        # bare is behind).
        if git -C "$WORKTREE" merge-base --is-ancestor "$bare_head" "$work_head"; then
            : # worktree is ahead, just push
        elif git -C "$WORKTREE" merge-base --is-ancestor "$work_head" "$bare_head"; then
            # bare is strictly ahead — fast-forward worktree.
            #
            # DATA-LOSS GUARD: `git reset --hard` silently discards uncommitted
            # changes to tracked files. The bare repo can advance without this
            # loop having pushed it — e.g. a teammate or CI pushing straight to
            # the bare repo — so a developer mid-edit would have that work
            # wiped. Refuse to hard-reset a dirty worktree: stall and let them
            # commit. Once committed, the worktree has a local commit ahead of
            # bare, so the next iteration integrates the change via rebase
            # (which is dirty-safe) instead. We `continue` to skip the push
            # below too — otherwise --force-with-lease would rewind the bare
            # repo back over that commit. Untracked files are intentionally
            # ignored: reset --hard leaves them, so they're not at risk and
            # shouldn't stall us.
            # Classify the worktree precisely. `git diff --quiet` exits 0
            # (clean), 1 (real changes), or >1 (git error — e.g. an index too
            # corrupt for heal_index_if_corrupt to rebuild this round). The old
            # `! git diff --quiet` collapsed >1 into "true", so a transient
            # corruption was misreported as uncommitted work and stalled here
            # forever (issue #4). Capture the codes without tripping `set -e`.
            unstaged=0; git -C "$WORKTREE" diff --quiet 2>/dev/null || unstaged=$?
            staged=0; git -C "$WORKTREE" diff --cached --quiet 2>/dev/null || staged=$?
            if [ "$unstaged" -gt 1 ] || [ "$staged" -gt 1 ]; then
                echo "$(date '+%F %T') - worktree index unreadable (git diff errored); skipping fast-forward, retrying in 10s (next loop rebuilds the index)"
                sleep 10
                continue
            elif [ "$unstaged" -ne 0 ] || [ "$staged" -ne 0 ]; then
                echo "$(date '+%F %T') - bare advanced to $bare_head but worktree has uncommitted changes; NOT hard-resetting (would lose work). Commit to integrate the bump; retrying in 10s."
                sleep 10
                continue
            fi
            echo "$(date '+%F %T') - fast-forwarding worktree to $bare_head"
            git -C "$WORKTREE" reset --hard "$bare_head"
            # Bare moved without us pushing (e.g. a teammate/CI push straight
            # to the bare repo). Flux's source may not have re-fetched yet, so
            # poke it now instead of waiting out the poll interval.
            should_trigger=1
        else
            # Genuine divergence. Try rebase.
            echo "$(date '+%F %T') - rebasing worktree on $bare_head"
            if ! git -C "$WORKTREE" rebase "$bare_head"; then
                git -C "$WORKTREE" rebase --abort || true
                echo "$(date '+%F %T') - REBASE CONFLICT: worktree and bare repo diverge."
                echo "$(date '+%F %T') -   Resolve manually: cd $WORKTREE && git pull --rebase $BARE_REPO_URL $branch"
                echo "$(date '+%F %T') - stalling 30s"
                sleep 30
                continue
            fi
        fi
    fi

    # Push the worktree to bare ONLY when our current HEAD differs from the bare
    # head we fetched this iteration. An idle loop (already in sync) or one that
    # just fast-forwarded onto the bare head (handled above, where HEAD ==
    # bare_head here) has nothing to push — and a force-with-lease push is a
    # full smart-HTTP negotiation we'd otherwise pay every POLL_INTERVAL for
    # nothing (issue #6). re-read HEAD here because the rebase/fast-forward
    # above may have moved it.
    cur_head=$(git -C "$WORKTREE" rev-parse HEAD 2>/dev/null || echo "")
    if [ -n "$cur_head" ] && [ "$cur_head" != "$bare_head" ]; then
        # --force-with-lease so a concurrent push we *didn't* see (very
        # unlikely in 2s) doesn't get clobbered.
        if ! git -C "$WORKTREE" -c core.hooksPath=/dev/null push \
            --force-with-lease="$branch:$bare_head" \
            "$BARE_REPO_URL" "$branch:$branch" 2>/dev/null; then
            # First push to a brand-new branch in the bare repo: no remote ref
            # yet, force-with-lease has nothing to compare to. Fall back to a
            # plain push (which creates the branch).
            git -C "$WORKTREE" -c core.hooksPath=/dev/null push "$BARE_REPO_URL" "$branch:$branch" 2>/dev/null || \
                echo "$(date '+%F %T') - warning: push failed"
        fi
        # Our push advanced the bare repo → Flux has new source state to pull,
        # so poke it. (The fast-forward branch above sets the flag for the
        # bare-advanced-without-us case, where we skip the push but still need
        # the trigger.)
        should_trigger=1
    fi
    # HEAD now equals the commit we want Flux to converge on — the pushed
    # commit, or (fast-forward case) the change we just pulled.
    [ "$should_trigger" = "1" ] && trigger_reconcile

    sleep "$POLL_INTERVAL"
done
