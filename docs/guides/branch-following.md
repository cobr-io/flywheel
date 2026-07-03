# Branch following & `flywheel use`

A sync loop keeps each worktree in sync with the in-cluster bare repo Flux
reads — `git-auto-sync-<app>` for app worktrees, `git-deploy-controller` for
the gitops repo (see [The gitops-repo loop](gitops-loop.md)). Part of that is
**branch following**: deploying the branch you're working on. How aggressively
it follows depends on the repo.

## App repos: automatic follow

For per-app syncs (`flywheel add app`), following is automatic: check out a
branch in the worktree and git-auto-sync repoints that app's `GitRepository` to
it, so you see the branch deploy. This is cheap and safe — an app repo carries
only its own workload.

## The gitops/self repo: opt-in follow

The repo that holds your `infra/`, `apps/`, and `builders/` is different.
Following its branch automatically is dangerous: a **transient** checkout — the
one `git rebase` does, for instance — would repoint Flux at a branch tip that
predates some infra, and because the infra Kustomization is `prune: true`, Flux
would **tear that infra down**. When the pruned resources are stateful or
finalizer'd (CNPG `Cluster`, tofu `Terraform`), the namespace can wedge in
`Terminating`. A few seconds of `git checkout` → minutes of manual repair.

So the gitops/self sync (`git-deploy-controller`) never follows checkouts. You
choose the deployed branch deliberately:

```
flywheel use <branch>      # deploy <branch>
flywheel use main          # switch back
```

`flywheel use` records the selection in a durable `flywheel.cobr.io/deploy-branch`
annotation on the self `GitRepository` (`flux-system/flux-system`) — and that's
all it does. Flux itself always tracks the constant deploy branch
`flywheel/local-deploy` and is never repointed; on its next tick (~2s),
`git-deploy-controller` reads the annotation, rebuilds the deploy branch from
your selection (plus the dev loop's image bumps), and pokes Flux.
`flywheel use <TAB>` completes with your local branches.

While a different branch is deployed you can still check out, rebase, and switch
branches freely in the worktree — the controller keeps mirroring your commits to
the bare repo but never changes which branch is deployed. Only `flywheel use`
does.

### Drift-correction: `flywheel up` won't silently move you

The annotation is the source of truth, re-read on every controller tick, and
it's written under its own field manager — so a later `flywheel up`, which
re-applies the self-source manifest, doesn't erase it. Nothing can quietly
change which branch feeds the cluster; only another `flywheel use` (or the
fallback below) moves it, and the controller logs when it does.

### Graceful degradation on delete

If the branch you deployed with `flywheel use` is later **deleted** from the
gitops repo, `git-deploy-controller` notices the selected branch no longer
exists and **falls back to the default branch** (the one the cluster was
bootstrapped on, typically `main`), logging the demotion — rather than leaving
Flux stuck on a vanished ref. Run `flywheel use <branch>` again to deploy a
different branch.
