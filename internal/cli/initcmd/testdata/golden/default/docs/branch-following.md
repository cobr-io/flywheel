# Branch following & `flywheel use`

git-auto-sync keeps each worktree in sync with the in-cluster bare repo Flux
reads. Part of that is **branch following**: pointing Flux's `GitRepository` at
the branch you're working on. How aggressively it follows depends on the repo.

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

So the gitops/self sync (`git-auto-sync-self`, env `AUTO_FOLLOW_BRANCH=false`)
does **not** follow checkouts. You choose the deployed branch deliberately:

```
flywheel use <branch>      # deploy <branch>
flywheel use main          # switch back
```

`flywheel use` records the selection in a durable `flywheel.cobr.io/deploy-branch`
annotation on the self `GitRepository` (`flux-system/flux-system`), repoints
`spec.ref.branch`, disables kustomize-controller reconcile so the change sticks,
and triggers an immediate reconcile. `flywheel use <TAB>` completes with your
local branches.

While a different branch is deployed you can still check out, rebase, and switch
branches freely in the worktree — git-auto-sync mirrors your commits to the bare
repo but never repoints Flux. Only `flywheel use` changes what's deployed.

### Drift-correction: `flywheel up` won't silently move you

The selection is the source of truth: git-auto-sync-self continuously reconciles
`spec.ref.branch` back to the `deploy-branch` annotation. So if something
clobbers the live branch — most importantly a later `flywheel up`, which
re-applies the self-source manifest with the bootstrap branch — the loop
re-asserts your selected branch (and logs it) instead of letting the cluster
quietly change under you. The annotation is written under its own field manager,
so `flywheel up` doesn't erase it.

### Graceful degradation on delete

If the branch you deployed with `flywheel use` is later **deleted** from the
gitops repo, git-auto-sync notices the selected branch no longer exists and
**falls back to the default branch** (the one the cluster was bootstrapped on,
typically `main`), logging the demotion — rather than leaving Flux stuck on a
vanished ref. Run `flywheel use <branch>` again to deploy a different branch.
