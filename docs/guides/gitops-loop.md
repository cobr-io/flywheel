# The gitops-repo loop

The [Adding apps guide](add-app.md) covers the loop for *application code*:
commit in an app worktree, a pod runs the new image. The gitops repo itself
gets the same treatment, minus the build: **edit a manifest, `git commit`, and
the live cluster converges in seconds** — no push, no CI, no image in the
path.

<picture>
  <source media="(prefers-color-scheme: dark)" srcset="../assets/gitopsloop-dark.svg">
  <img alt="The gitops-repo loop: you edit manifests and commit → git-deploy-controller pushes your branch to the in-cluster git-server and maintains the flywheel/local-deploy branch → kustomize-controller applies your overlays, pruning what the commit removed → the cluster matches your repo seconds later" src="../assets/gitopsloop-light.svg" width="720">
</picture>

## What happens on commit

1. You commit a change under `apps/`, `infra/`, or `builders/` on your branch.
2. The `git-deploy-controller` pod (in `flywheel-system`, ticking every 2s)
   notices the worktree advanced over the `/workspaces` bind-mount, pushes
   your branch to the in-cluster git-server, rebuilds the deploy branch
   (below), and pokes Flux so it fetches now instead of on its next interval.
3. Flux's `client-infra`, `client-builders`, and `client-apps` Kustomizations
   apply your overlay paths — including SOPS-decrypting any `*.enc.yaml`
   secrets, and **pruning** whatever your commit removed.

The same commit-driven convergence a real cluster gets from a git push —
that's the production-parity point. Two consequences worth internalising:

* **A commit is enough; a push is not needed.** Flux watches the in-cluster
  mirror of your repo, not your origin. Pushing is for your teammates.
* **A commit is also required.** Uncommitted edits never deploy — the
  controller mirrors commits, not your dirty working tree.

## The deploy branch: `flywheel/local-deploy`

Flux does not track your branch directly. It permanently tracks a
machinery-owned branch, `flywheel/local-deploy`, which `git-deploy-controller`
rebuilds as **your selected branch + the dev loop's image-bump commits**. This
is why:

* `flux get sources git` reports revisions on `flywheel/local-deploy` — a
  branch you never created. That's expected.
* your own branch history stays clean: the app loop's automated image bumps
  land on the deploy branch, never on yours, so you don't rebase around
  machine commits.

Treat the branch as machinery: don't commit to it, don't base work on it,
don't delete it (it's rebuilt on the next tick anyway).

## Which branch is deployed

Deliberately chosen, never inferred: a `git checkout` (or the transient
checkouts a rebase does) never changes what the cluster runs. Switch with:

```sh
flywheel use <branch>   # deploy that branch
flywheel use main       # and back
```

See [Branch following & `flywheel use`](branch-following.md) for the details
and the reasoning.

## Deletes are deploys too

The client Kustomizations run with `prune: true`, so removing a resource from
the repo and committing removes it from the cluster. That's the loop working
as intended — iterate on infra by editing YAML, not by `kubectl delete`. It
also means a commit that (transiently) drops infra **tears that infra down**;
with stateful components (databases, finalizer-heavy operators) that can wedge
a namespace. Prefer additive commits while iterating on stateful infra.

The mirror image also holds: **out-of-band edits don't stick**. A
`kubectl edit` on a Flux-owned object is drift, and Flux reverts it within the
reconcile interval. If you want a change to persist, it goes in git — that's
the point.

## When something's off

```sh
flux get kustomizations                                    # client-* Ready?
flux get sources git flux-system                           # revision advancing on commit?
kubectl logs -n flywheel-system deploy/git-deploy-controller
```

* **Committed but nothing changes** — check the git-deploy-controller logs:
  your commit may not be reaching the in-cluster mirror (and remember:
  *committed*, not just saved).
* **Kustomization `Ready: False`** — the message names the broken manifest;
  fix and commit again. A kustomize build error blocks that whole tier until
  it's fixed.
* **A resource keeps reverting** — you're editing the cluster instead of the
  repo; make the change in git.
