# Bringing the cluster up without flywheel

`flywheel up` gives you the fast local dev loop. This guide is the escape hatch:
how to bring the **same local cluster** up with stock [Flux](https://fluxcd.io)
and `kubectl`, using **no `flywheel` binary at all**. You give up the fast loop
(no in-cluster build server, image automation, or branch-following) — in return,
you prove the repo isn't captive: `apps/` and `infra/` are plain Kustomize, and
everything Flux needs is a handful of stock YAML files you write and own.

This is what makes flywheel a convenience layer rather than a lock-in. The tool
doesn't ship a committed escape hatch — it doesn't need to. The gitops repo is
ordinary Kustomize, so a plain Flux entrypoint is something you can author by
hand in a few minutes, as shown below.

## What flywheel does (and why there's nothing to "turn off")

`flywheel up` renders its own Flux entrypoint to a tmpdir at runtime and applies
it imperatively — it points the cluster at an in-cluster git mirror and wires up
the dev-loop build server, image automation, and branch-following. None of that
machinery is committed to your repo, and `flywheel up` never reconciles
`clusters/`. So going flywheel-free isn't a matter of removing anything: you just
author a normal Flux entrypoint that points at your **GitHub remote** instead,
and apply it yourself.

## Author the entrypoint

Create `clusters/local/flux-system/` with the five files below. These are stock
Flux objects — no flywheel-specific resources, no in-cluster mirror, no
`flywheel/local-deploy` branch. The source is **GitHub on `main`**, so the
cluster reconciles whatever you've pushed (there's no fast loop reflecting
uncommitted local edits — that's the trade).

`source.yaml` — where the cluster reconciles from. Replace `<org>/<repo>` with
your real remote:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: flux-system
  namespace: flux-system
spec:
  interval: 1m            # slow on purpose — no fast loop, don't hammer the remote
  url: https://github.com/<org>/<repo>.git
  ref:
    branch: main          # matches git.integration_branch in flywheel.yaml
  # Private repo? Create a `flux-system` secret (see step 3) and uncomment:
  # secretRef:
  #   name: flux-system
```

`namespaces.yaml` — the application namespace (matches `namespaces.apps` in
`flywheel.yaml`):

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: apps
  labels:
    kubernetes.io/metadata.name: apps
```

`infra.yaml` — the client infra tier (`infra/overlays/local`):

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: client-infra
  namespace: flux-system
spec:
  interval: 1m
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  path: ./infra/overlays/local
  decryption:               # decrypts *.enc.yaml with the sops-age key (step 2)
    provider: sops
    secretRef:
      name: sops-age
```

`apps.yaml` — the client apps tier (`apps/overlays/local`), reconciled after
infra:

```yaml
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: client-apps
  namespace: flux-system
spec:
  interval: 1m
  prune: true
  sourceRef:
    kind: GitRepository
    name: flux-system
  path: ./apps/overlays/local
  dependsOn:
    - name: client-infra
  decryption:
    provider: sops
    secretRef:
      name: sops-age
```

`kustomization.yaml` — an aggregator so the whole thing applies in one shot:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ./namespaces.yaml
  - ./source.yaml
  - ./infra.yaml
  - ./apps.yaml
```

> There's no `flywheel-infra` or `flywheel-system` layer here — those ship
> flywheel's own dev-loop TLS/traefik wiring from the in-cluster mirror. A
> vanilla cluster uses whatever ingress its distribution provides.

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

# 4. Apply the entrypoint you authored. Flux takes over and reconciles apps/ + infra/.
kubectl apply -k clusters/local/flux-system
```

Watch it converge:

```sh
flux get kustomizations --watch
# client-infra and client-apps should reach Ready=True.
```

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
runtime — so the directory you authored just sits in the repo, harmless, until
you ask for it. You can keep it committed as a permanent escape hatch, or delete
it once you've proven the bring-up; either way the knowledge is yours now.
