# Design: `flywheel up` from a git worktree

**Status:** proposed
**Author:** Matthijs van der Kroon
**Date:** 2026-07-03
**Issue:** #62

## Problem

Running `flywheel up` from a directory that is a **git linked worktree**
(`git worktree add`) rather than a normal clone leaves every `client-*`
Kustomization stuck at `Ready=False — "Source artifact not found"`. flywheel's
own tiers come up, so `up` exits 0 (with only a `WARN step 14: Flux
Kustomizations not all Ready before deadline`), but nothing the user authored
deploys.

**Root cause.** A worktree's `.git` is a **file**, not a directory:

```
gitdir: /Users/<user>/…/<mainrepo>/.git/worktrees/<id>
```

That is an **absolute host path**. The in-cluster `git-deploy-controller` only
has `workspaces_root` bind-mounted (at `/workspaces`), so `/Users/…/.git`
doesn't exist inside the container. Every `git -C /workspaces/<repo> …` the
controller runs (`internal/selfsync/selfsync.go`) then can't resolve the repo's
objects/refs, so `PushAuthored` fails every tick with:

```
authored branch "<b>" is not present in worktree /workspaces/<repo>
```

The client repo's in-cluster bare repo is seeded **exclusively** by this
controller (there is no host-side seed of the client repo — `up` step 11c only
pushes the *flywheel mirror*). So the `flux-system` GitRepository never gets an
artifact → every `client-*` Kustomization fails with "Source artifact not
found".

## Layout taxonomy

`flywheel up` can be launched from four layouts. The fix must handle each:

| # | Layout | `.git` | Common git-dir | Sibling model | Target behavior |
|---|--------|--------|----------------|--------------|-----------------|
| **A** | Normal clone | dir | self-contained | ✓ | works today — unchanged |
| **B** | **Sibling worktree** (issue #62: `efq-gitops-feat` next to `efq-gitops`) | file | elsewhere on host, shareable into the VM | ✓ (checkout is a direct child of `workspaces_root`) | **Support** — bind-mount the common git-dir at its host-abs path |
| **C** | **Nested worktree** (e.g. `<repo>/.claude/worktrees/agent-x`) | file | an *ancestor* of the checkout | ✗ — `workspaces_root` (= parent of checkout) lands *inside* a repo | **Fail-fast** with a message naming the nesting (escape hatch to override) |
| **D** | Worktree whose common git-dir can't bridge into k3d (under `/tmp`, `/var/folders`, or outside the VM's shares) | file | not shareable | — | **Fail-fast** with remediation (use a full clone) |

Note the terminology clash: flywheel already uses "worktree" for an app's
sibling checkout under `workspaces_root` (`internal/cli/worktree`). This doc's
"worktree" means a *git linked worktree*. The new code lives in a distinct
package, `internal/cli/gitcheckout`, to keep them separate.

## Why a mount, and why it's enough

The worktree's `.git` file records an **absolute** host path to the shared git
dir. Inside the container only `/workspaces` (and explicit host-abs mounts)
exist, so the only way to make `git` resolve a worktree — short of rewriting the
user's `.git` file, which would corrupt their host checkout — is to bind-mount
the shared git-dir into the k3d node **at that same absolute host path**.

Verified locally: with the common dir present at its recorded absolute path,
`git -C <otherpath> rev-parse/show-ref/for-each-ref refs/heads/<b>` all succeed
even when the checkout is accessed from a *different* path than git's own
bookkeeping records (exactly the container's `/workspaces/<repo>` vs host-path
mismatch). Worktrees share all `refs/heads/*` via the common dir, so every
branch the controller might treat as AUTHORED is resolvable — the "single-branch
checkout" concern in the issue is moot once the common dir is mounted.

This is a live mount, not a one-time seed: the controller keeps reading the
developer's new commits each tick, so a snapshot copy would go stale. Mounting
is the right primitive.

## Approach

Add `internal/cli/gitcheckout` — a small, pure-logic-plus-guard package
mirroring `internal/cli/hostmount`:

```go
// Layout classifies the git layout a directory represents.
type Layout struct {
    Dir          string // inspected dir (abs)
    IsWorktree   bool   // .git is a file → a git linked worktree
    CommonDir    string // abs host path of the shared git dir; "" for a clone
    MainWorktree string // abs host path of the main working tree
    Nested       bool   // checkout lives INSIDE the main working tree
}

func Inspect(dir string) (Layout, error) // stat .git; if a file, resolve via git
```

Wire it into three points, matching the existing `hostmount` model (hard guard
in `up`, soft check in `doctor`):

1. **`up` step 2 (pre-flight, after doctor quick-checks).** Inspect the layout.
   If it's a **nested** worktree (C), fail fast with remediation, unless the
   escape hatch `FLYWHEEL_ALLOW_NESTED_WORKTREE` is set. Inspect errors are
   non-fatal (treat as a clone; the quick-check already asserts `git`).

2. **`up` step 7 (cluster create).** For any worktree, pass the resolved
   `CommonDir` to k3d. `k3d.CreateClusterOpts` gains `GitCommonDir string`;
   `CreateCluster` adds `--volume <dir>:<dir>@server:*` and `@agent:*` when set.
   (Volumes are fixed at create time; `up` recreates a stopped cluster and
   no-ops a running one, so the mount lands on the fresh create that the repro
   performs.)

3. **`up` step 7b (visibility probe).** Extend the existing bind-mount check:
   for a worktree, also probe that `CommonDir` is visible inside the node
   (`k3d.NodePathExists`). If not (D), fail fast with remediation instead of the
   cryptic downstream "Source artifact not found".

4. **`flywheel doctor` (full).** Add `worktreeCheck(cwd)`: pass for a clone or a
   supported sibling worktree; fail (informational) for a nested worktree or a
   common-dir on an unshareable temp path (reusing
   `hostmount.UnshareableTempDir`).

No change to the controller (`selfsync`) or bootstrap: for a sibling worktree
`ResolveRepoBaseName` already yields the worktree's basename, so the bare-repo
name, `WORKTREE`, and the `flux-system` GitRepository URL stay consistent.

## Scope

- **In:** the **gitops repo** checkout being a sibling worktree (B), with hard
  guards on nested (C) and non-shareable (D). This is issue #62's exact case.
- **Out (deferred):** app *source* repos declared in `flywheel.yaml` being
  worktrees; making nested worktrees (C) first-class. Both are edge machinery
  beyond the reported problem — see Open questions.

## Test plan

- `gitcheckout` unit tests: clone → `IsWorktree=false`; a real
  `git worktree add` sibling → `IsWorktree=true`, `CommonDir` resolves,
  `Nested=false`; a nested `git worktree add <repo>/sub/x` → `Nested=true`.
  (Table-driven, creating throwaway repos in `t.TempDir()`.)
- `k3d.CreateClusterOpts` → arg builder includes the two `--volume` flags iff
  `GitCommonDir != ""` (extract a pure `createClusterArgs` for assertion).
- `up` pre-flight: nested worktree returns the fail-fast error; the escape hatch
  bypasses it. (Exercised via a fake repo dir + env.)
- doctor: `worktreeCheck` pass/fail cases.
- Manual e2e: `git worktree add ../<repo>-feat <branch>` then `flywheel up` from
  it → `client-*` Kustomizations reconcile to Ready.

## Open questions

1. **Support nested worktrees (C) as first-class?** It is *technically* possible
   — the same common-dir mount resolves a nested worktree's git — but nesting
   also breaks the sibling model (separate app repos won't be found under the
   nested `workspaces_root`; `/workspaces` becomes a surprising hidden subtree;
   the bare repo is named after the worktree id). Making it clean means
   special-casing `workspaces_root` up to the main repo's parent plus bare-repo
   naming and app discovery. Deferred behind the escape hatch for now; revisit
   if dogfooding from `.claude/worktrees/` becomes a real need.
2. Should app-source worktrees get the same treatment? Same mechanism, more
   surface (one common-dir mount per worktree app). Deferred.
