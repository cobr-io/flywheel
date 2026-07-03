# Adding apps

Once `flywheel up` has your cluster running, the next step is putting your own
services on it. There is one question to answer first, and it decides
everything that follows:

> **Do you build this image yourself, or consume it off the shelf?**

* **You build it** (it's your code, you want commits to land in a pod) →
  [`flywheel add app`](#home-grown-flywheel-add-app). The CLI scaffolds both a
  build pipeline and a workload, and the dev loop takes over.
* **You consume it** (a public Helm chart, an OCI-packaged chart, a prebuilt
  image) → [hand-author the manifests](#off-the-shelf-no-builder) under
  `apps/base/<name>/`. No CLI involved, no builder — Flux reconciles whatever
  you commit.

Cluster-wide operators and platform services (a database operator, a metrics
stack) follow the same fork but live under `infra/` instead of `apps/` — see
the `infra/README.md` in your repo.

## Home-grown: `flywheel add app`

### 1. Put the source next to the gitops repo

Flywheel bind-mounts one directory — `workspaces_root` — into the cluster, and
every app worktree must be a **direct child** of it. By default
`workspaces_root` is the parent directory of your gitops repo, which makes app
repos *siblings* of the gitops repo:

```
~/src/
├── acme-gitops/     # your flywheel repo (flywheel.yaml at the root)
└── acme-api/        # app worktree — sibling, with a Dockerfile
```

Three ways to get a worktree there:

* **you already have the project checked out** as a sibling — done;
* **you're starting a new project** — create it there (`git init` it; the only
  hard requirement is a `Dockerfile`);
* **the project lives on a remote** — pass the git URL straight to
  `add app` and it clones into `workspaces_root` for you (see below).

A nested checkout (inside another repo) or a directory outside
`workspaces_root` is refused — the cluster's single bind-mount can't reach it.
Sibling `git worktree add ../acme-api-feat` checkouts work.

### 2. Run it

From the gitops repo root:

```sh
flywheel add app acme-api        # bare name = child of workspaces_root
flywheel add app ../acme-api     # relative or absolute paths work too
flywheel add app git@github.com:acme/acme-api.git   # or clone it for you
```

`<dir>` tab-completes to the git worktrees under `workspaces_root`. In clone
mode, `--branch` picks the branch to check out (default: the remote's default
branch).

The app name is derived from a project manifest in the worktree
(`package.json`, `pyproject.toml`, `setup.cfg`, `go.mod`, `Cargo.toml`,
`composer.json`, `pom.xml`, `*.gemspec`), falling back to the directory name;
`--name` overrides. The command pre-flights the Dockerfile and refuses to
scaffold an app that could never build.

One run scaffolds **both halves** and wires them in:

* `builders/base/<name>/` — the build pipeline: a Flux `GitRepository`
  watching the in-cluster mirror of your worktree, a `build-config` ConfigMap
  (what to build), a `git-auto-sync` Deployment (mirrors your worktree's
  commits into the cluster), and an `ImageRepository` + `ImagePolicy` (rolls
  new tags out). A README in the folder explains each piece.
* `apps/base/<name>/` — the workload: a Deployment / Service / Ingress
  serving at `https://<name>.<local.domain>/`.

It also records the worktree in the `workspace:` block of `flywheel.yaml` —
that's how a teammate's `flywheel up --clone` knows to materialise the same
worktree on their machine.

### Flags

| Flag | Default | Reach for it when |
|---|---|---|
| `--name` | derived from a project manifest, else the dir name | the derived name is wrong, or several apps share one worktree |
| `--image` | the app name | the image should be named differently from the app |
| `--context` | `.` | monorepo: the docker build context is a subdirectory |
| `--dockerfile` | `Dockerfile` | the Dockerfile lives elsewhere within `--context` |
| `--target` | the Dockerfile's last stage | you want a specific multi-stage build target |
| `--namespace` | `namespaces.apps` from `flywheel.yaml` | the app needs its own namespace (a managed Namespace object is created for it) |
| `--branch` | the remote's default branch | clone mode only: which branch to check out and record |

For a **monorepo**, run `add app` once per service against the same worktree,
each with its own `--name`/`--image` and `--context` pointing at that
service's subdirectory.

### 3. Commit, and what to expect

The scaffold lands **uncommitted** in your gitops repo. Review it, then:

```sh
git add -A && git commit -m "add acme-api"
```

Locally, a commit is all Flux needs — `git-auto-sync` mirrors your gitops
repo's commits into the in-cluster git-server, which Flux watches. Pushing to
your real remote is for your teammates, not for the cluster.

Then, in order:

1. **Within `flux.interval_local`** (default 10s) Flux applies the new
   manifests. The pod will sit in `ImagePullBackOff` at first — the scaffolded
   Deployment references a `:0-placeholder` tag until the first build lands.
   This is normal.
2. **The first build starts** from your worktree's current HEAD (no extra
   commit needed) and pushes `<name>:<timestamp>-<sha>` into the local
   registry. A first build does a cold `docker build` of your image — expect
   minutes, not seconds.
3. **Image automation rewrites the tag** in `apps/base/<name>/deployment.yaml`
   and the pod rolls out. Your app is up at
   `https://<name>.<local.domain>:<https_port>/` — `add app` prints the exact
   URL. (Reaching it in a browser also needs local name resolution — see the
   [Local DNS guide](local-dns.md).)
4. **From now on, every commit in the worktree becomes a running pod in
   seconds.** That's the dev loop.

> **The scaffold assumes your container listens on port 8080.** If your app
> serves on a different port, edit `containerPort` and the Service
> `targetPort` in `apps/base/<name>/deployment.yaml`. Everything scaffolded is
> plain YAML, yours to edit — replicas, env vars, probes.

### Local-only apps and `publish-app`

`add app` records where the source came from. A worktree with an `origin`
remote is recorded by URL; one without is recorded as **`local_only`** — its
source exists only on your machine, so nobody else (and no other cluster)
could ever reconcile it.

That distinction drives a guard:

* On the **integration branch** (`git.integration_branch`, default `main`),
  registering a local-only app is **refused** outright — switch to a feature
  branch first.
* On a **feature branch** it proceeds with a warning: publish before this
  branch merges.

Publishing is two steps, once the project is ready to share:

```sh
cd ../acme-api
git remote add origin git@github.com:acme/acme-api.git
git push -u origin main

cd ../acme-gitops
flywheel publish-app acme-api
```

`publish-app` verifies the worktree's `origin` is reachable **and** its
current branch is actually pushed, then flips the `workspace:` entry from
`local_only` to the origin URL. Commit that change with your branch; the app
can now merge to the integration branch, and teammates' `up --clone` can
materialise it.

### When something's off

What "working" looks like, and where to look when it isn't:

```sh
flux get kustomizations              # client-apps / client-builders Ready?
kubectl get pods -n apps             # the app pod
kubectl get jobs -n flywheel-system  # build jobs: build-<name>-<ts>-<sha>
```

* **Pod stuck on `0-placeholder` / `ImagePullBackOff` for long** — the first
  build hasn't landed. Check the build Job's logs
  (`kubectl logs -n flywheel-system job/<build-job>`); a broken Dockerfile
  fails here, not at `add app` time.
* **No build Job at all** — check the git-auto-sync pod for your app in
  `flywheel-system`: your worktree's commits may not be reaching the
  in-cluster mirror. Note it mirrors **commits** — uncommitted edits never
  deploy.
* **Pod runs but the URL doesn't resolve** — that's browser-side name
  resolution, see the [Local DNS guide](local-dns.md).
* **Builds fail / pods `Pending` on a large repo** — the in-cluster git-server
  can hit its memory limit; raise `git_server.memory_limit` in
  `flywheel.yaml`.

## Off-the-shelf: no builder

For anything you don't build yourself there is nothing to scaffold — you're
just adding Flux-reconciled manifests. Create `apps/base/<name>/`, list it in
`apps/base/kustomization.yaml`, commit. **Nothing under `builders/` is ever
involved.**

A worked example — [podinfo](https://github.com/stefanprodan/podinfo) from its
public Helm chart. `apps/base/podinfo/helmrelease.yaml`:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata:
  name: podinfo
  namespace: apps
spec:
  interval: 1h
  url: https://stefanprodan.github.io/podinfo
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: podinfo
  namespace: apps
spec:
  interval: 10m
  chart:
    spec:
      chart: podinfo
      version: ">=6.0.0"
      sourceRef:
        kind: HelmRepository
        name: podinfo
  values:
    ingress:
      enabled: true
      className: traefik
      hosts:
        - host: podinfo.localdev.me   # <name>.<local.domain> from flywheel.yaml
          paths:
            - path: /
              pathType: Prefix
      tls:
        - hosts:
            - podinfo.localdev.me
```

`apps/base/podinfo/kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ./helmrelease.yaml
```

Then add `  - ./podinfo` under `resources:` in `apps/base/kustomization.yaml`
and commit. Flux's helm-controller (installed by `flywheel up`) fetches and
installs the chart within the reconcile interval; the app serves at
`https://podinfo.<local.domain>:<https_port>/` like any other.

The other off-the-shelf shapes work the same way, only the source differs:

* **OCI-packaged chart** — an `OCIRepository` (pointing at the `oci://…` chart
  ref) plus a `HelmRelease` referencing it via `chartRef`.
* **Prebuilt public image** — a plain Deployment / Service / Ingress
  referencing `nginx:1.27` or whatever, no chart at all. Crib the shape from
  any `add app`-scaffolded `apps/base/<name>/deployment.yaml` and drop the
  `$imagepolicy` comment (there's no build to automate).
* **Plain upstream manifests** — save them into the folder and list them as
  kustomize `resources:`.

Off-the-shelf apps land in the default apps namespace like everything else;
if one needs its own namespace, add a Namespace object to
`apps/base/namespaces.yaml` and list it in `apps/base/kustomization.yaml`
(the same convention `add app --namespace` uses).
