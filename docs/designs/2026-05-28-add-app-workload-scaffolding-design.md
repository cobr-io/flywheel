# `flywheel add-app` workload scaffolding

**Status:** approved
**Date:** 2026-05-28
**Author:** matthijs (with collaboration from Claude)

## Problem

Today, taking a freshly-initialised flywheel repo to a running app at
`https://<app>.<local-domain>` takes eight manual steps:

1. `mkdir <myapp>`
2. `flywheel init <myapp>`
3. `cd <myapp> && edit flywheel.yaml.local` (per-dev image overrides; dogfood only)
4. `flywheel up`
5. `flywheel add-app <name>`
6. **manually** create `apps/base/<name>/{deployment.yaml,kustomization.yaml}` and append `<name>` to `apps/base/kustomization.yaml`
7. `git commit && git push`
8. wait for Flux to pull and apply

Step 6 is the dominant pain. `add-app` scaffolds the **builder** half
(`builders/base/<name>/` — the Kaniko pipeline + ImageRepository +
ImagePolicy) but explicitly does NOT scaffold the **workload** half
(`apps/base/<name>/`). This is flagged as out-of-scope in
`internal/cli/addapp/addapp.go:8` and in the `NextSteps` message at
`addapp.go:116`. The user is left to hand-write a Deployment whose image
ref *must* include the `{"$imagepolicy": "flux-system:<name>"}` marker for
Flux image-update-automation to bump tags — an easy thing to forget or
get wrong.

Step 3 is dogfood-specific (per-developer `flywheel.images.*` overrides
for engineers working on flywheel itself); a regular user never touches
`.local`. `workspaces_root` is already auto-detected as `dirname(repoDir)`
(`up.go:402-408`), so it's not actually required. **Step 3 is out of scope
for this design** and remains a one-time-per-repo hand edit.

Steps 7-8 (commit and wait for Flux) are intentional — explicit git is
part of the GitOps contract and we don't want to bypass it.

## Approach

Extend `flywheel add-app` to render a second template tree —
`apps/base/<name>/` — containing a Deployment + Service + Ingress wired
to the local mkcert TLS setup, and append the entry to
`apps/base/kustomization.yaml`. Pure conventions, no new CLI flags. After
this change, the eight-step flow collapses to:

1. `mkdir <myapp>`
2. `flywheel init <myapp>`
3. *(dogfood only)* edit `flywheel.yaml.local`
4. `flywheel up`
5. `flywheel add-app <name>` ← now scaffolds **both** halves
6. `git commit && git push`
7. app comes live at `https://<name>.<local-domain>`

Considered alternatives:

* **Split into `add-builder` + `add-workload` with `add-app` as a wrapper.**
  More composable, more CLI surface to document and test, no concrete use
  case for the split today. Rejected as premature factoring.
* **Unify into a single template that mirrors the destination tree
  (`builders/<name>/...` and `apps/<name>/...`).** Would require teaching
  `render.Tree` a destination-mapping mode. Larger refactor with no
  immediate payoff. Rejected.

## Components

### New embedded files

`manifests/apps-template/deployment.yaml.tmpl` — single multi-doc file
with Deployment + Service + Ingress (mirrors the existing hand-written
`apps/base/tmpapp/deployment.yaml` shape).

`manifests/apps-template/kustomization.yaml.tmpl` — references
`./deployment.yaml`.

Both are picked up automatically by the existing `embed.go` `Assets` FS
directive that covers `manifests/...`.

### Modified file

`internal/cli/addapp/addapp.go`:

* Add a constant for the new sub-FS path (`manifests/apps-template`)
  alongside the existing per-app-template sub-FS.
* In `Run`, **before** any render, pre-flight stat both
  `builders/base/<name>` and `apps/base/<name>`. Error if either exists.
  (Today only the builders dir is checked.)
* After the existing builders render + builders kustomization append:
  * Render `apps-template` into `apps/base/<name>/`.
  * Append `  - ./<name>` to `apps/base/kustomization.yaml`.
* Generalise `appendBuilder(repoDir, name)` → `appendResource(path, name)`
  (path is the absolute kustomization file to mutate). Call it for both
  files. The append logic (idempotent, handles `resources: []` → block
  sequence transition) stays unchanged.
* Add `LocalDomain: cfg.Local.Domain` to `buildValues`.
* Validate `cfg.Local.Domain != ""` in `Run`; error early if missing.
* Update `Result.NextSteps` from *"Edit apps/base/<name>/deployment.yaml
  manually..."* to *"commit and push; Flux will pull and apply within
  cfg.Flux.IntervalLocal"*.

No new CLI flags. No changes to `cmd/flywheel/main.go`. No changes to
`init` or the client skeleton (which already ships
`apps/base/kustomization.yaml` with `resources: []`).

## Template contents

`manifests/apps-template/deployment.yaml.tmpl`:

```yaml
# App workload scaffolded by `flywheel add-app`. The image tag is a Flux
# image-update-automation target: image-builder-controller pushes
# `{{ .AppName }}:<ts>-<sha>` tags into the local registry; the
# `{"$imagepolicy": "flux-system:{{ .AppName }}"}` marker tells Flux to
# rewrite this line when a newer tag appears. Edit replicas/env/etc here.
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ .AppName }}
  namespace: {{ .AppsNamespace }}
  labels:
    app: {{ .AppName }}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: {{ .AppName }}
  template:
    metadata:
      labels:
        app: {{ .AppName }}
    spec:
      containers:
        - name: {{ .AppName }}
          image: {{ .RegistryURL }}/{{ .ClientName }}/{{ .AppName }}:0-placeholder # {"$imagepolicy": "flux-system:{{ .AppName }}"}
          ports:
            - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: {{ .AppName }}
  namespace: {{ .AppsNamespace }}
spec:
  selector:
    app: {{ .AppName }}
  ports:
    - port: 80
      targetPort: 8080
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ .AppName }}
  namespace: {{ .AppsNamespace }}
spec:
  ingressClassName: traefik
  # No `secretName` under tls: Traefik's TLSStore.default
  # (kube-system/local-cert, provisioned by `flywheel up` step 13) serves
  # the wildcard *.{{ .LocalDomain }} cert. Listing the host here just
  # attaches the route to the websecure entrypoint.
  tls:
    - hosts:
        - {{ .AppName }}.{{ .LocalDomain }}
  rules:
    - host: {{ .AppName }}.{{ .LocalDomain }}
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: {{ .AppName }}
                port:
                  number: 80
```

`manifests/apps-template/kustomization.yaml.tmpl`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
# App workload for {{ .AppName }}, rendered by `flywheel add-app`.
resources:
  - ./deployment.yaml
```

### Conventions baked in

* `containerPort: 8080` and Service `port: 80 → targetPort: 8080`. Matches
  what the user has been writing by hand. Users edit the YAML if they
  need different ports.
* `replicas: 1`.
* Ingress host `<AppName>.<LocalDomain>` — derived; not a flag.
* Image ref pattern `{RegistryURL}/{ClientName}/{AppName}:0-placeholder`.
  The placeholder tag matches the ImagePolicy regex
  (`^\d+-[a-f0-9]+$`) so the controller doesn't reject it; the marker
  comment ensures image-update-automation overwrites the entire line
  once the builder produces a real tag. Pod will `ImagePullBackOff`
  until the first build completes — same as today's hand-written
  workflow.
* No `tls.secretName`. Traefik's `TLSStore.default` (`kube-system/local-cert`)
  serves the wildcard `*.<LocalDomain>` cert as the SNI default.
  Verified live on 2026-05-28: a probe Ingress with this shape served
  TLSv1.3 to `https://whoami.localdev.me:8540` with the mkcert cert,
  validated against the system trust store (no `-k` needed).

## Error handling

* **Pre-flight existence check.** Stat both `builders/base/<name>` and
  `apps/base/<name>` before any render. Error if either exists, listing
  both paths in the message. Prevents leaving the repo half-scaffolded
  if the second render fails.
* **`apps/base/kustomization.yaml` must exist.** `init` writes it; if
  missing, surface the same "missing `resources:` key" error shape as
  the existing builders append helper.
* **`cfg.Local.Domain` empty.** Error early in `Run`. Would otherwise
  produce an invalid Ingress host of `<name>.`.
* **Idempotency.** The `appendResource` helper retains the existing
  skip-if-already-present behavior. Re-running `add-app <existing-name>`
  errors at the pre-flight (refuses to overwrite directories) — same
  UX as today.
* **No transactional rollback.** If the apps render fails after the
  builders render succeeded, the builders dir + builders kustomization
  entry are left behind. Pre-flight makes this rare; full rollback would
  need staging into a tmpdir and an atomic swap — deferred as YAGNI.

## Test plan

Extend `internal/cli/addapp/addapp_test.go`:

* **`TestRun_RendersAppsTemplate`** — assert
  `apps/base/<name>/deployment.yaml` and
  `apps/base/<name>/kustomization.yaml` are written, with substituted
  values (AppName, AppsNamespace, RegistryURL, LocalDomain present in
  expected positions). Asserts the `{"$imagepolicy": "flux-system:<name>"}`
  marker is present verbatim.
* **`TestRun_AppendsToAppsKustomization`** — start with
  `apps/base/kustomization.yaml` containing `resources: []`, run
  `add-app foo`, assert it becomes block-sequence with `  - ./foo`. Run
  again with `add-app bar`, assert both entries present.
* **`TestRun_RefusesIfAppsDirExists`** — pre-create
  `apps/base/<name>/`, assert `Run` errors before touching
  `builders/base/<name>/`.
* **`TestRun_ErrorsOnMissingLocalDomain`** — fixture with empty
  `local.domain`, assert error.

Existing tests in `addapp_test.go` (`TestRun_RendersBuilderTemplate` and
friends) should continue to pass unchanged since the apps render is
purely additive.

The TLS shape was validated live on the running cluster on 2026-05-28
(throwaway `tls-probe` namespace, `traefik/whoami` backend, curl with
system trust store). No new automated integration test is added; the
existing `manifests/dev-loop/` scenarios already exercise the full
add-app → Flux → running pod path with hand-written tmpapp manifests.
Migrating dev-loop to use `flywheel add-app` for its scaffolding is a
small follow-up, out of scope for this design.

## Migration

None. `add-app` is purely additive for existing repos:

* Repos already using `add-app` keep their builders unchanged.
* Repos with hand-written `apps/base/<name>/` (e.g. the dogfood `myapp`
  with `tmpapp`) keep working; they predate the new scaffold and the
  pre-flight existence check protects them from accidental overwrite.

## Open questions

* Should `dev-loop` scenarios migrate from hand-written tmpapp manifests
  to `flywheel add-app`-generated ones in a follow-up? (Likely yes, to
  keep one source of truth for the scaffold shape, but not a blocker.)
* Future: per-app `--port` or `--type=worker|cronjob` flags if the
  zero-flag default proves too restrictive. Defer until evidence
  appears.
