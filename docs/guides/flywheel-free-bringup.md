# Bringing the cluster up without flywheel

`flywheel up` gives you the fast local dev loop. This guide is the escape hatch:
how to bring the **same local cluster** up with stock [Flux](https://fluxcd.io)
and `kubectl`, using **no `flywheel` binary at all**. You give up the fast loop
(no in-cluster build server, image automation, or branch-following) — in return,
the repo proves it isn't captive: everything Flux needs is committed YAML you
own.

This is what makes flywheel a convenience layer rather than a lock-in. If
flywheel ever went away, this path still brings your apps up.

## What's committed

`flywheel init` writes a vanilla Flux entrypoint at
[`clusters/local/flux-system/`](../clusters/local/flux-system/):

| File | What it is |
| --- | --- |
| `source.yaml` | `GitRepository/flux-system` → your GitHub remote, `main` branch |
| `infra.yaml` | `Kustomization/client-infra` → `infra/overlays/local` |
| `apps.yaml` | `Kustomization/client-apps` → `apps/overlays/local` (after infra) |
| `namespaces.yaml` | the `apps` namespace |
| `kustomization.yaml` | aggregator so the whole thing applies in one `kubectl apply -k` |

These are stock Flux objects — no flywheel-specific resources, no in-cluster git
mirror, no `flywheel/local-deploy` branch. The source is **GitHub on `main`**, so
the cluster reconciles whatever you've pushed (there's no fast loop reflecting
uncommitted local edits — that's the trade).

## Prerequisites

- A Kubernetes cluster. Any will do; for parity with flywheel's local
  environment just create a plain k3d one: `k3d cluster create`.
- The [`flux` CLI](https://fluxcd.io/flux/installation/) and `kubectl`, both
  pointed at that cluster.
- The committed local SOPS key at `clusters/local/age.key` (shipped in the repo).

## Bring-up

```sh
# 1. Install the Flux controllers (this creates the flux-system namespace).
flux install

# 2. Load the SOPS decrypt key so the Kustomizations can decrypt *.enc.yaml.
#    Required before step 4 even if you ship no encrypted secrets yet.
kubectl create secret generic sops-age \
  -n flux-system \
  --from-file=age.agekey=clusters/local/age.key

# 3. (Private repo only) give Flux credentials to read your remote.
#    Skip this for a public repo.
flux create secret git flux-system \
  --url=https://github.com/<org>/<repo>.git \
  --username=<user> --password=<personal-access-token>

# 4. Apply the entrypoint. Flux takes over and reconciles apps/ + infra/.
kubectl apply -k clusters/local/flux-system
```

Watch it converge:

```sh
flux get kustomizations --watch
# client-infra and client-apps should reach Ready=True.
```

> **Check the remote URL first.** `flywheel init` runs `git init` on a fresh repo
> with no `origin`, so `source.yaml`'s `url:` is a best-effort guess
> (`github.com/<org>/<name>`). If your real remote differs, edit that one line —
> it's your YAML.

## Images

There's one gap to know about. App manifests scaffolded by `flywheel add app`
ship a **dev-loop image placeholder**:

```yaml
image: registry.localhost:5000/myapp:0-placeholder # {"$imagepolicy": "flux-system:myapp"}
```

That `:0-placeholder` tag is rewritten to a real, freshly-built tag only by
flywheel's in-cluster image automation — which is exactly the machinery you're
not running here. Vanilla Flux will reconcile the tier **green** (the manifests
are valid), but the pods can't pull a placeholder tag, so they stay
`ImagePullBackOff` until you give them a real image.

In a no-flywheel world that's your normal CI's job: your pipeline builds and
pushes images to a registry, and the committed manifests reference those real
tags. To stand the default scaffold up end-to-end without flywheel, replace the
placeholder with a pullable reference, e.g.:

```yaml
image: ghcr.io/<org>/myapp:<tag>   # a tag your CI actually published
```

(You can drop the `# {"$imagepolicy": ...}` marker too — it's inert without the
automation. Leaving it does no harm.)

## Going back to flywheel

This entrypoint and `flywheel up` are mutually exclusive — both want to own the
cluster's `flux-system` `GitRepository`. They don't run at once; switching is
just applying the other one:

- **To flywheel:** `flywheel up` (re-points the source at the in-cluster mirror).
- **To vanilla:** `kubectl apply -k clusters/local/flux-system` (re-points it at
  GitHub).

`flywheel up` never touches `clusters/` — it renders its own entrypoint at
runtime — so this directory just sits in the repo, harmless, until you ask for
it.
