# apps/

Deployable application workloads for the **acme** cluster —
Deployments, Services, Ingresses, and anything else that serves your traffic.
(Cluster-wide operators and platform services go in `../infra/` instead.)

## Layout

* `apps/base/<name>/` — one folder per app, holding its resources.
* `apps/base/kustomization.yaml` — lists each app folder under `resources:`.
* `apps/overlays/local/` — assembles `../../base` for the cluster; Flux's
  `client-apps` Kustomization reconciles this path.

## Adding an app

Start with one question: **do you build the image yourself, or consume it
off the shelf?**

### You build it (active development)

Run `flywheel add app <worktree>`. It scaffolds the workload here under
`apps/base/<name>/` **and** the matching build pipeline under
`builders/base/<name>/`, and wires both into their `kustomization.yaml`.
See [../builders/README.md](../builders/README.md) for what the builder
half does.

### You consume it off the shelf (a public chart or prebuilt image)

No builder is involved. Add the app's resources under `apps/base/<name>/`
and list the folder in `apps/base/kustomization.yaml`:

* a public Helm chart — a `HelmRepository` (the chart source) plus a
  `HelmRelease` (the install);
* an OCI-packaged chart — an `OCIRepository` plus a `HelmRelease`;
* plain upstream manifests — reference them as kustomize `resources`.

Off-the-shelf apps never need anything under `../builders/`.
