# builders/

Per-app build pipelines — **only for images you build yourself**. If you
consume something off the shelf (a public Helm chart or a prebuilt image),
it does **not** belong here: put it under `../apps/` (an application) or
`../infra/` (a cluster-wide operator) instead.

## Layout

* `builders/base/<name>/` — one folder per image you build, normally
  created by `flywheel add app`.
* `builders/base/kustomization.yaml` — lists each builder folder under
  `resources:`.
* `builders/overlays/local/` — assembles `../../base` for the cluster.

## What a builder folder contains

`flywheel add app <worktree>` scaffolds these (all hand-editable afterwards):

* **GitRepository** (`flywheel-system`) — Flux watches the in-cluster bare
  repo for your worktree and triggers a build on each commit.
* **build-config** ConfigMap (`flywheel-system`) — declares what to build
  (image name, build context, Dockerfile); add entries for a monorepo.
* **git-auto-sync** Deployment (`flywheel-system`) — syncs your host
  worktree to the in-cluster bare repo and follows branch switches.
* **ImageRepository** + **ImagePolicy** (`flux-system`) — Flux image
  automation watches the registry and selects the newest tag to roll out.

## Adding one by hand

Create `builders/base/<name>/` with the resources above, then add
`  - ./<name>` to `builders/base/kustomization.yaml`. Running
`flywheel add app` does all of this for you, including the matching
`../apps/base/<name>/` workload.
