#!/usr/bin/env bash
# Local-only app guard. A worktree declared `local_only` in flywheel.yaml's
# workspace block has source that exists only on one developer's machine, so
# any app building from it must never reach the integration branch — other
# developers and the cluster could never reconcile it.
#
# This blocks (exit non-zero) when the effective target branch is the
# integration branch and any app builds from a local-only worktree; otherwise
# it warns. It is the single detection codepath shared by the pre-commit hook
# and CI.
#
# Effective target branch: $GITHUB_BASE_REF when set (a PR's base branch in
# GitHub Actions), else the current branch. Integration branch: read from
# flywheel.yaml (git.integration_branch), default "main".
#
# Depends on mikefarah's yq (https://github.com/mikefarah/yq).
#
# Bash 3.2 compatible (no associative arrays, no mapfile) — this runs as a
# pre-commit hook and in CI, and stock macOS ships /bin/bash 3.2.

set -euo pipefail

# Integration branch (default main).
integ="main"
if [ -f flywheel.yaml ]; then
  v="$(yq '.git.integration_branch' flywheel.yaml 2>/dev/null || echo "null")"
  if [ -n "$v" ] && [ "$v" != "null" ]; then
    integ="$v"
  fi
fi

# Effective target branch.
target="${GITHUB_BASE_REF:-}"
if [ -z "$target" ]; then
  target="$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "")"
fi

# Worktrees the workspace block flags local_only, as a newline-delimited
# list. Bash 3.2 has no associative arrays, so membership below is checked
# with `grep -Fx` instead of a hash lookup.
local_only_wt=""
if [ -f flywheel.yaml ]; then
  while IFS= read -r name; do
    [ -n "$name" ] && [ "$name" != "null" ] || continue
    local_only_wt="${local_only_wt}${name}
"
  done < <(yq '.workspace.repos[] | select(.local_only == true) | .name' flywheel.yaml 2>/dev/null || true)
fi

is_local_only_wt() {
  [ -n "$1" ] && printf '%s' "$local_only_wt" | grep -Fxq "$1"
}

# Apps (builders) whose worktree — the basename of spec.url minus .git — is
# flagged local_only.
local_only=()
for f in builders/base/*/gitrepository.yaml; do
  [ -f "$f" ] || continue
  url="$(yq '.spec.url' "$f" 2>/dev/null || echo "")"
  [ -n "$url" ] && [ "$url" != "null" ] || continue
  wt="$(basename "$url")"
  wt="${wt%.git}"
  if is_local_only_wt "$wt"; then
    local_only+=("$(basename "$(dirname "$f")") (worktree $wt)")
  fi
done

if [ "${#local_only[@]}" -eq 0 ]; then
  echo "local-only guard: no local-only apps."
  exit 0
fi

if [ "$target" = "$integ" ]; then
  echo "ERROR: local-only app(s) on the integration branch ('$integ'): ${local_only[*]}" >&2
  echo "  Their source exists only on one machine and cannot be reconciled elsewhere." >&2
  echo "  Publish each first: push its worktree to a remote, then run 'flywheel publish-app <name>'." >&2
  exit 1
fi

echo "WARNING: local-only app(s) present: ${local_only[*]}" >&2
echo "  Publish them ('flywheel publish-app <name>') before this branch merges to '$integ'." >&2
exit 0
