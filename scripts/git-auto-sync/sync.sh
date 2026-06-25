#!/bin/bash
# Bidirectional sync between a worktree (host-mounted) and a bare repo
# (served by the in-cluster git-server).
#
# Loop:
#   1. Detect current branch in worktree; if it changed, patch the Flux
#      GitRepository to track the new branch. Disabled when
#      AUTO_FOLLOW_BRANCH=false (the gitops/self sync): the deployed branch is
#      then chosen explicitly via `flywheel use`, and this step only degrades a
#      deleted selected branch back to DEFAULT_BRANCH (issue #17).
#   2. git fetch the bare repo's view of <branch>.
#   3. git rebase the worktree onto FETCH_HEAD. This pulls Flux Image
#      Automation Controller's manifest bumps (which it commits to the bare
#      repo over HTTP) back into the developer's local checkout. For sibling
#      repos this is a no-op because IAC never writes to them.
#   4. git push the worktree's current branch to the bare repo.
#
# A rebase conflict means the developer touched an image-tag line at the
# same time as IAC. We abort the rebase, log loudly, and stall for 30s
# before retrying — the developer sees the stall in `kubectl logs` and
# resolves it by `git pull --rebase` in their worktree (which also clears
# the cluster-side state, since the next push fast-forwards).
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
# or we fetched IUA's image-tag bump), also poke Flux to reconcile immediately
# instead of waiting out a poll interval. Annotating the source — and, for the
# gitops/self sync, the apps Kustomization — with reconcile.fluxcd.io/requestedAt
# collapses stacked poll latency out of the commit-to-pod loop. Flux treats the
# annotation value as opaque; it only has to change between triggers.
KUSTOMIZATION_NAME="${KUSTOMIZATION_NAME:-}"
KUSTOMIZATION_NAMESPACE="${KUSTOMIZATION_NAMESPACE:-flux-system}"
RECONCILE_ANNOTATION="reconcile.fluxcd.io/requestedAt"

# Branch-follow mode (issue #17).
#   true  (default; app repos): repoint the Flux GitRepository to whatever
#         branch the worktree is on — switch branches, see them deploy.
#   false (the gitops/self sync): do NOT auto-follow checkouts. Auto-following
#         is dangerous on the repo that carries infra: a transient checkout —
#         e.g. the one `git rebase` does — would repoint Flux at an infra-less
#         branch tip and, with prune:true, tear that infra down. With follow
#         off, the deployed branch is chosen explicitly via `flywheel use
#         <branch>`; this loop only mirrors commits to the bare repo and never
#         repoints on a checkout.
AUTO_FOLLOW_BRANCH="${AUTO_FOLLOW_BRANCH:-true}"
# When follow is off, the branch to fall back to if the explicitly selected
# (Flux-tracked) branch is deleted from the gitops repo. Graceful degradation:
# repoint Flux here and log, rather than leaving the source stuck on a vanished
# ref (issue #17).
DEFAULT_BRANCH="${DEFAULT_BRANCH:-main}"
# Annotation `flywheel use` writes on the GitRepository to record the selected
# branch. In opt-in mode this is the source of truth the loop reconciles
# spec.ref.branch to (drift-correction). Must match usecmd.DeployBranchAnnotation.
DEPLOY_BRANCH_ANNOTATION="flywheel.cobr.io/deploy-branch"

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

# Read an annotation value off the GitRepository ($1 = annotation key). Used in
# opt-in mode to read the operator's `flywheel use` selection. Empty if absent.
# Bracket notation handles the dots and slash in the annotation key.
gitrepository_annotation() {
    kubectl get gitrepository "$GITREPOSITORY_NAME" \
        -n "$GITREPOSITORY_NAMESPACE" \
        -o "jsonpath={.metadata.annotations['$1']}" 2>/dev/null || true
}

# Update the deploy-branch annotation ($1 = branch). Used by the degrade path so
# the recorded selection converges with the fallback (otherwise the loop would
# re-log the degrade every iteration). Best-effort.
set_deploy_annotation() {
    kubectl annotate gitrepository "$GITREPOSITORY_NAME" \
        -n "$GITREPOSITORY_NAMESPACE" \
        "${DEPLOY_BRANCH_ANNOTATION}=$1" --overwrite >/dev/null 2>&1
}

# Optional: the ImageUpdateAutomation that writes image bumps to THIS
# repo's bare clone (only the gitops/self git-auto-sync sets these — IAC
# never writes to app repos). When set, the IUA is suspended for the
# duration of a branch switch so it can't commit to the bare repo while
# we're repointing GitRepository.spec.ref.branch. This closes design
# Open Issue #11 ("branch switch with stale IAC commit in flight"): the
# branch switch and IAC become mutually exclusive, so a feature-branch
# commit can't leak onto another branch through a racing fetch/rebase.
IUA_NAME="${IUA_NAME:-}"
IUA_NAMESPACE="${IUA_NAMESPACE:-flux-system}"
IUA_SUSPENDED=0

set_iua_suspend() {  # $1 = true|false
    [ -n "$IUA_NAME" ] || return 0
    if kubectl patch imageupdateautomation "$IUA_NAME" -n "$IUA_NAMESPACE" \
        --type=merge -p "{\"spec\":{\"suspend\":$1}}" >/dev/null 2>&1; then
        echo "$(date '+%F %T') - IUA $IUA_NAME/$IUA_NAMESPACE suspend=$1"
    else
        echo "$(date '+%F %T') - warning: IUA $IUA_NAME suspend=$1 failed (will retry)"
        return 1
    fi
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
    # Poke Flux to reconcile now; called after the bare repo head changes,
    # with $1 = the commit SHA we want Flux to converge on. Best-effort: a
    # failed annotate just falls back to the normal poll interval, so warn but
    # never abort the sync loop.
    local target_sha="$1"
    local now
    now="$(date -u '+%FT%TZ')"
    # 1. Poke the source so source-controller fetches the new commit now.
    if ! kubectl annotate gitrepository "$GITREPOSITORY_NAME" \
        -n "$GITREPOSITORY_NAMESPACE" \
        "${RECONCILE_ANNOTATION}=${now}" --overwrite >/dev/null 2>&1; then
        echo "$(date '+%F %T') - warning: reconcile trigger on GitRepository '$GITREPOSITORY_NAME' failed (will rely on poll)"
    fi
    [ -n "$KUSTOMIZATION_NAME" ] || return 0

    # 2. Wait for the source artifact to actually advance to target_sha before
    #    poking the Kustomization. Poking both at once races the fetch: the
    #    Kustomization reconciles against the *stale* artifact, marks our
    #    request handled, and then defers the real apply to a later natural
    #    reconcile — observed as a ~60s stall on the last hop. Waiting for the
    #    artifact first guarantees kustomize-controller applies the new commit
    #    on this poke. (kustomize-controller's own source-watch would
    #    eventually apply it too, but only after the next interval; this makes
    #    it immediate.) Bounded so a slow/failed fetch can't hang the loop.
    local i rev
    for i in $(seq 1 50); do
        rev=$(kubectl get gitrepository "$GITREPOSITORY_NAME" \
            -n "$GITREPOSITORY_NAMESPACE" \
            -o jsonpath='{.status.artifact.revision}' 2>/dev/null || true)
        case "$rev" in
            *"$target_sha"*) break ;;
        esac
        sleep 0.2
    done
    now="$(date -u '+%FT%TZ')"
    if ! kubectl annotate kustomization "$KUSTOMIZATION_NAME" \
        -n "$KUSTOMIZATION_NAMESPACE" \
        "${RECONCILE_ANNOTATION}=${now}" --overwrite >/dev/null 2>&1; then
        echo "$(date '+%F %T') - warning: reconcile trigger on Kustomization '$KUSTOMIZATION_NAME' failed (will rely on poll)"
    fi
}

LAST_BRANCH=""

# Clear any stale suspend left by a previous crashed instance (the
# resume below is gated on IUA_SUSPENDED, so without this an IUA could be
# stuck suspended forever after a mid-switch crash).
set_iua_suspend false || true

echo "$(date '+%F %T') - starting bidirectional sync: $WORKTREE <-> $BARE_REPO_URL"

while true; do
    branch=$(current_branch)

    if [ -z "$branch" ] || [ "$branch" = "HEAD" ]; then
        echo "$(date '+%F %T') - detached or empty branch, skipping"
        sleep "$POLL_INTERVAL"
        continue
    fi

    # Resume IAC once a switch has settled: we suspended it on the switch,
    # the new branch is now the tracked one (patch succeeded), and we've
    # done at least one sync iteration on it. Done at the top so it still
    # runs after an early `continue` in the previous iteration — making
    # the suspend window self-closing.
    if [ "$IUA_SUSPENDED" = "1" ] && [ "$branch" = "$LAST_BRANCH" ]; then
        set_iua_suspend false && IUA_SUSPENDED=0
    fi

    if [ "$AUTO_FOLLOW_BRANCH" != "true" ]; then
        # Opt-in follow (the gitops/self sync, issue #17): never repoint Flux
        # from a worktree checkout — the deployed branch is chosen explicitly
        # via `flywheel use <branch>`, which records it in the durable
        # DEPLOY_BRANCH_ANNOTATION on the GitRepository. Two jobs here:
        #
        #   1. Drift-correction: reconcile spec.ref.branch to that annotation.
        #      An external clobber — notably a `flywheel up` re-applying the
        #      self-source manifest with the default branch — would otherwise
        #      silently change the deployed branch. We re-assert the selected
        #      branch so `up` can't quietly move the cluster (issue #17).
        #   2. Graceful degradation: if the selected branch has been deleted
        #      from the gitops repo, fall back to DEFAULT_BRANCH (which always
        #      exists), updating the annotation too so we converge and stop
        #      logging. No selection yet (empty annotation) → leave whatever the
        #      bootstrap manifest set; nothing to assert.
        desired=$(gitrepository_annotation "$DEPLOY_BRANCH_ANNOTATION")
        if [ -n "$desired" ]; then
            if [ "$desired" != "$DEFAULT_BRANCH" ] \
                && ! git -C "$WORKTREE" show-ref --verify --quiet "refs/heads/$desired"; then
                echo "$(date '+%F %T') - selected branch '$desired' no longer exists in the gitops repo; degrading to default '$DEFAULT_BRANCH'"
                set_deploy_annotation "$DEFAULT_BRANCH" || true
                patch_gitrepository "$DEFAULT_BRANCH" || true
            else
                live=$(gitrepository_branch)
                if [ -n "$live" ] && [ "$live" != "$desired" ]; then
                    echo "$(date '+%F %T') - drift: GitRepository on '$live' but selected branch is '$desired'; re-asserting (e.g. after a flywheel up re-apply)"
                    patch_gitrepository "$desired" || true
                fi
            fi
        fi
    elif [ "$branch" != "$LAST_BRANCH" ]; then
        echo "$(date '+%F %T') - branch is '$branch' (was '${LAST_BRANCH:-<none>}')"
        # Suspend IAC BEFORE repointing the branch so it can't commit to
        # the bare repo mid-switch (Open Issue #11). Resumed at the top of
        # the next iteration once the new branch is synced.
        set_iua_suspend true && IUA_SUSPENDED=1
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
        # poll interval. No IUA suspend dance here: the worktree branch is
        # unchanged, so there's no switch race for IAC to leak across — we're
        # only restoring the value, not migrating between branches. Cheap in
        # steady state: one `kubectl get`, and a patch only when drift is seen.
        live_branch=$(gitrepository_branch)
        if [ -n "$live_branch" ] && [ "$live_branch" != "$branch" ]; then
            echo "$(date '+%F %T') - drift: GitRepository on '$live_branch' but worktree on '$branch'; re-asserting"
            patch_gitrepository "$branch" || true
        fi
    fi

    # Pull anything the bare repo has that we don't (IAC commits).
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
            # changes to tracked files. On the gitops/self worktree the bare
            # repo advances on its own every time Image-Update-Automation commits
            # an image-tag bump (i.e. after every app build), so a developer
            # mid-edit — e.g. an uncommitted `flywheel add-app` that appended to
            # builders/ + apps/ kustomization.yaml — would have that work wiped.
            # Refuse to hard-reset a dirty worktree: stall and let them commit.
            # Once committed, the worktree has a local commit ahead of bare, so
            # the next iteration integrates the bump via rebase (which is
            # dirty-safe) instead. We `continue` to skip the push below too —
            # otherwise --force-with-lease would rewind the bare repo back over
            # the IUA bump. Untracked files are intentionally ignored: reset
            # --hard leaves them, so they're not at risk and shouldn't stall us.
            if ! git -C "$WORKTREE" diff --quiet || ! git -C "$WORKTREE" diff --cached --quiet; then
                echo "$(date '+%F %T') - bare advanced to $bare_head but worktree has uncommitted changes; NOT hard-resetting (would lose work). Commit to integrate the bump; retrying in 10s."
                sleep 10
                continue
            fi
            echo "$(date '+%F %T') - fast-forwarding worktree to $bare_head"
            git -C "$WORKTREE" reset --hard "$bare_head"
            # Bare moved without us pushing (typically IUA's image-tag bump on
            # the gitops repo). Flux's source may not have re-fetched yet, so
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

    # Push worktree to bare. Use --force-with-lease so a concurrent IAC commit
    # we *didn't* see (very unlikely in 2s) doesn't get clobbered.
    if ! git -C "$WORKTREE" -c core.hooksPath=/dev/null push \
        --force-with-lease="$branch:$bare_head" \
        "$BARE_REPO_URL" "$branch:$branch" 2>/dev/null; then
        # First push to a brand-new branch in the bare repo: no remote ref
        # yet, force-with-lease has nothing to compare to. Fall back to a
        # plain push (which creates the branch).
        git -C "$WORKTREE" -c core.hooksPath=/dev/null push "$BARE_REPO_URL" "$branch:$branch" 2>/dev/null || \
            echo "$(date '+%F %T') - warning: push failed"
    fi

    # If our push advanced the bare repo (current HEAD differs from the bare
    # head fetched at the top of this iteration), Flux has new source state to
    # pull — poke it. The fast-forward branch above sets the flag for the
    # IUA-commit case.
    new_head=$(git -C "$WORKTREE" rev-parse HEAD 2>/dev/null || echo "")
    if [ -n "$new_head" ] && [ "$new_head" != "$bare_head" ]; then
        should_trigger=1
    fi
    # HEAD now equals the commit we want Flux to converge on — the pushed
    # commit, or (fast-forward case) the IUA bump we just pulled.
    [ "$should_trigger" = "1" ] && trigger_reconcile "$(git -C "$WORKTREE" rev-parse HEAD 2>/dev/null || echo "")"

    sleep "$POLL_INTERVAL"
done
