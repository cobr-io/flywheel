# Flywheel: a reusable template for per-client GitOps repos

**Status:** approved
**Date:** 2026-05-15 (revised 2026-05-19: rename to Flywheel; bootstrap via local clone; declarative reconcile with `--yes`; copier-style update merge; port allocator; `flywheel.yaml.local`; reference-client-only v0.1; minimal OS deps — embedded kubectl/flux/sops/kustomize/git/yaml-diff. Further 2026-05-19 review pass: mirror Flywheel manifests in-cluster for true offline; per-developer age key in `~/.config`; archive answer-values for `update` 3-way merge; CLI applies as `flux-controller`; inotify via privileged DaemonSet; CRDs tiered as never-auto-delete; `new` populates cache; require `docker` CLI; pin Flux GitRepository by SHA; drop `prod/` scaffolding in v0.1; `schema: v1alpha1` during v0.x; canonicalise key order only in snapshots; `doctor` before clone; `.local` arrays replace wholesale; `flywheel allocator gc`; synthetic `flywheel-dogfood` for Phase 3. 2026-05-27 dogfood pass: rename `new` → `init` with cwd-or-path semantics; promote `add-app` from Phase 3 to v0.1; binary now `go:embed`s skeleton + manifests + per-app-template so no git clone of github.com/cobr-io/flywheel is required at runtime; `flywheel.images.{git-server,git-auto-sync,image-builder-controller}` config map in `flywheel.yaml` lets clients pin per-image refs, with `.local` the natural place for per-developer dogfood overrides; `up` auto-runs `k3d image import` for refs found in the host docker store; option (c) — unset override + no published image = hard fail with remediation pointer.)
**Author:** matthijs (with collaboration from Claude)

> **Update (2026-06-10): multi-profile support removed.** This document
> describes two local TLS profiles — `mkcert` (default) and
> `tailscale-le-wildcard`. The `tailscale` profile and the entire
> profile-selection mechanism have since been removed: Flywheel now ships a
> single, hardcoded `mkcert` TLS setup. `local.profile` / `local.tailscale`
> are no longer in `flywheel.yaml`, the `--profile` / `--mkcert` flags are
> gone, `manifests/infra/overlays/local-*` collapsed into `manifests/infra/`,
> and `flywheel clean --crds` (which only reaped the tailscale profile's
> operator CRDs) is removed. Read every "profile" reference below as
> historical context. See the CHANGELOG entry dated 2026-06-10.

## Problem

`reference-gitops` carries a working sub-30-second local dev loop (`git-server` +
`git-auto-sync` + `image-builder-controller` + Kaniko + Flux Image Automation —
see [`2026-05-13-local-dev-loop-design.md`](2026-05-13-local-dev-loop-design.md)).
The pattern is good enough to reuse across multiple customer GitOps repos.
Known target clients today:

* **reference-gitops** itself — small, will be the reference client and the
  integration canary for every Flywheel release.
* **The data-platform client** (`example-org/dataplatform-apps`) — mature
  EKS + local setup with a domain-specific operator (five CRDs) and a heavier
  infra surface (Strimzi Kafka, Altinity ClickHouse,
  MinIO, kube-prometheus-stack, AWS Load Balancer Controller, cert-manager with
  LE-DNS01-Route53, external-dns). Uses an older docker-builder (DinD + Python)
  that rewrites image tags in YAML — wholesale replacement, not a refactor.
* **A self-hosted client** — single-cluster k3d on a bare-metal ArchLinux host
  with NVIDIA GPU, a production-grade Tailscale operator pattern (single proxy
  pod on tailnet + LE wildcard cert), CNPG, MinIO+restic backups to a NAS.

Two requirements drive this work:

1. Spinning up a *new* client GitOps repo should be a single-command bootstrap
   (no copy-rename-edit chore, no per-client divergence accumulating on day one).
2. When the dev loop is improved, that improvement must propagate to N existing
   client repos without N rounds of `git merge` conflict-resolution.

We evaluated three options for the relationship between Flywheel and a
per-client repo:

* **Option 1 — forever-linked git fork.** Client is a `git fork` of Flywheel;
  improvements arrive via `git merge upstream/main`. Cheap to start, but every
  literal client name (`reference-`, ports, namespaces) becomes a merge conflict
  hotspot forever unless Flywheel is aggressively parameterised — at which
  point you've done most of option 3's work anyway.
* **Option 3a — submodule.** Flywheel as a `flywheel/` git submodule. Same total
  surface; submodule UX hurts in practice.
* **Option 3b — pinned Flywheel (chosen).** Flywheel publishes pre-built
  container images on ghcr.io and a Flux-consumable manifest tree at a git tag.
  Client repos are thin — they own apps, per-app builder folders, overlays, and
  a `flywheel.yaml` answers file; they consume Flywheel via a Flux remote
  `GitRepository` and per-tag image pins. Bumping Flywheel is a single line
  change to `flywheel.yaml`.

## Goals

1. **One-command bootstrap** for a new client: `flywheel init` (in `cd
   my-gitops`) writes a working thin client repo in-place, and
   `flywheel init <path>` is the sibling variant. `flywheel up` brings
   the local k3d cluster online with the full dev loop in under two
   minutes on a warm machine.
2. **One-line Flywheel bump** for an existing client: edit
   `flywheel.yaml: version: vX.Y.Z`, commit, push. Flux re-resolves Flywheel
   remote source; pods roll on next reconcile.
3. **Zero client-specific literals** in Flywheel. Everything client-specific
   (cluster name, registry name, ports, namespaces, ingress hosts) lives in
   `flywheel.yaml` and propagates via a `flywheel-config` ConfigMap and Kustomize
   variables.
4. **First-class local profiles.** v0.1 ships two: `mkcert` (current reference-client
   pattern, default) and `tailscale-le-wildcard` (self-hosted pattern, advanced).
   Profile is selected at `new` time and switchable later by editing
   `flywheel.yaml` and re-running `flywheel up`.
5. **Greenfield-easy migration for the reference client.** Explicit non-goal in v0.1:
   support the data-platform client. Its domain operators (Strimzi/ClickHouse/a domain-specific operator)
   stay in the data-platform client's repo forever; its EKS prod surface (AWS Load Balancer
   Controller, external-dns, cert-manager prod issuer, IRSA) lands in the v0.2+
   optional-components catalog.

**v0.1 client scope is the reference client only.** The self-hosted client and the data-platform client inform the design but
neither migrates in v0.1 — the self-hosted client needs CNPG/MinIO/GPU components in the
v0.2+ catalog, the data-platform client needs the operators + EKS prod surface. The Problem section
above frames the three repos because their *patterns* shape v0.1; the *clients*
shipped against v0.1 are the reference client plus one synthetic-greenfield dogfood client
(Phase 3).

## Non-goals

* Multi-tenant clusters. One k3d cluster per client is assumed in v0.1.
* Multi-environment Flywheel semantics. v0.1 only writes `clusters/local/`
  and `*/overlays/local/`; a `flywheel new-env prod` subcommand and real
  multi-env semantics arrive in v0.2+ when the data-platform client migrates.
* The optional-components catalog (CNPG, kube-prometheus-stack, MinIO,
  registry-GC, prod-stub-aws, gpu-nvidia, etc.). Catalog is v0.2+ — v0.1 ships
  only what the reference client and the self-hosted client need.
* `flywheel plan` (terraform-style dry-run). Useful once the diff surface
  grows; not in v0.1.
* Air-gap mode. Some clients can't reach ghcr.io; a `--mirror-from=<internal>`
  flag arrives in v0.2+.
* Per-app folders as Kustomize components (3b-flavour from the brainstorm).
  v0.3+, once a client has 5+ apps.

## Prerequisites

Flywheel ships as a single static Go binary and embeds Go libraries for
everything that can be done in-process. The required external surface is
deliberately minimal.

**Required on every host** (`flywheel doctor` fails fast if missing):

| Tool | Why | Install (macOS) |
|------|-----|-----------------|
| `git` | The user's own GitOps repo workflow (Flywheel's whole purpose) | Bundled with Xcode CLT, or `brew install git` |
| `k3d` | The local cluster runtime | `brew install k3d` |
| `docker` CLI + daemon | k3d needs a daemon; Flywheel's image-mirror step shells to the `docker` CLI (`docker pull/tag/push`) | `brew install --cask docker` (Docker Desktop ships both); or `brew install colima docker` to combine Colima daemon with the CLI; or `brew install podman docker` |

**Conditional, profile-only:**

| Tool | Profile | Why required |
|------|---------|--------------|
| `mkcert` | `mkcert` (default) | OS trust-store integration for local HTTPS without browser warnings. Touching the system keychain from a portable Go binary is per-OS and needs elevated privileges — not worth reimplementing. `brew install mkcert`. |

**Embedded in the Flywheel binary — NOT required as OS packages:**

| What's embedded | Replaces external CLI |
|-----------------|----------------------|
| `k8s.io/client-go` | `kubectl` for apply / get / delete / wait |
| `sigs.k8s.io/kustomize/api` | `kustomize build` |
| Flux install manifests pinned to a known version, applied via client-go | `flux install` |
| `getsops/sops/v3` library | `sops` (age decryption only — no GPG/cloud KMS in v0.1) |
| yaml-aware diff library (or fall back to `git diff --no-index`) | `dyff`, `yq` for migration validation |
| `go-git` for cache clones | shelling out to `git clone` for the `~/.cache/flywheel/<version>/` populate |

Net effect: a fresh Mac with `git`, `k3d`, and a `docker` CLI + daemon
(Docker Desktop, or Colima/podman with a separately-installed `docker` CLI)
can run `flywheel up` end-to-end for the `tailscale` profile with no further
installs. The `mkcert` profile adds a single `brew install mkcert` (one-time).
The user's own host-level `git` is used by their normal repo workflow;
Flywheel itself uses embedded `go-git` for its caches, so the host `git`
version doesn't constrain Flywheel.

## Architecture

```
┌─────────────────────────────── developer Mac ──────────────────────────────┐
│                                                                            │
│  ~/src/github.com/<customer-org>/<customer>-gitops/                        │
│  ├── flywheel.yaml          # version pin + client params                  │
│  ├── flywheel.yaml.local    # gitignored: host paths, port overrides       │
│  ├── apps/                  # 100% client                                  │
│  ├── builders/base/<app>/   # per-app builder folders (cp'd from           │
│  │                          #   Flywheel template; hand-edited v0.1)       │
│  ├── infra/overlays/        # client patches on Flywheel overlay           │
│  └── clusters/local/flux-system/                                           │
│      ├── flywheel-source.yaml         # GitRepo -> in-cluster Flywheel    │
│      │                                #          mirror @ pinned SHA      │
│      ├── self-source.yaml             # GitRepo -> in-cluster git-srv     │
│      ├── flywheel-config.yaml         # ConfigMap from flywheel.yaml       │
│      ├── builders-kustomization.yaml  # flywheel//manifests/dev-loop       │
│      ├── infra-kustomization.yaml     # ./infra/overlays/local             │
│      └── apps-kustomization.yaml      # ./apps/overlays/local              │
│                                                                            │
│  k3d cluster `<client>-local`                                              │
│  (mounts ~/src/.../<customer-org>/ as /workspaces in the cluster)          │
└────────────────────────────────────────────────────────────────────────────┘
                                    │
          ┌──────────────── inside the cluster ────────────────┐
          │  namespace: `flywheel-system`                      │
          │                                                    │
          │  FLYWHEEL-OWNED  (from in-cluster Flywheel mirror  │
          │                   @ v0.1.0 SHA, via Flux           │
          │                   GitRepository):                  │
          │    git-server                 Deploy + Svc + PVC   │
          │    image-builder-controller   Deploy + RBAC        │
          │    kaniko-cache               PVC                  │
          │    image-update-automation    Flux IUA             │
          │    inotify-bump               privileged DaemonSet │
          │                               (one-shot sysctl)    │
          │    (local-profile: mkcert | tailscale manifests)   │
          │                                                    │
          │  CLIENT-OWNED  (from in-cluster git-server         │
          │                 serving the client's bare repo):   │
          │    per-app git-auto-sync Deployment (one per app)  │
          │    per-app GitRepository, ImageRepository,         │
          │      ImagePolicy, build-config ConfigMap           │
          │    apps/ workloads                                 │
          │                                                    │
          │  FLYWHEEL IMAGES (mirrored from ghcr.io to local   │
          │                   k3d registry on `flywheel up`):  │
          │    k3d-<client>-local-registry:5000/git-server     │
          │    k3d-<client>-local-registry:5000/git-auto-sync  │
          │    k3d-<client>-local-registry:5000/image-builder  │
          └────────────────────────────────────────────────────┘
```

Key mechanics:

* The in-cluster `git-server` hosts *two* bare repos: the client's own (as
  today) and a Flywheel mirror — `flywheel up` pushes the contents of
  `~/.cache/flywheel/<version>/` (at the pinned commit SHA) into a second bare
  repo on first bootstrap and on every Flywheel version bump.
* The client repo has *two* Flux `GitRepository` resources: `self-source`
  (serves the client's bare repo) and `flywheel-source` (serves the in-cluster
  Flywheel mirror). Neither points at GitHub at runtime — after first
  bootstrap, the cluster reconciles fully offline.
* Both `GitRepository` resources pin by commit SHA. `flywheel.yaml` carries
  the human-readable `flywheel.version: vX.Y.Z` tag; the CLI resolves the tag
  to a SHA at `up`/`update` time and writes that SHA into
  `flywheel-source.yaml: spec.ref.commit`. Tags-as-labels, SHAs-as-truth.
* The client's `builders-kustomization.yaml` references the Flywheel
  GitRepository via `path: ./manifests/dev-loop/overlays/local` — so the
  dev-loop manifests come from Flywheel side.
* The client's `infra-kustomization.yaml` references a tiny local file that
  *wraps* Flywheel's infra overlay
  (`resources: [flywheel://manifests/infra/overlays/local-<profile>]` plus any
  client patches).
* Flywheel images are public on ghcr.io. `flywheel up` pulls them once and
  re-pushes to the local k3d registry on each bootstrap, so cluster manifests
  reference `k3d-<client>-local-registry:5000/git-server:v0.1.0` — no runtime
  ghcr.io dependency. Combined with the in-cluster Flywheel mirror above,
  the dev loop is fully offline after first bootstrap.
* Parameters that vary per client (registry name, namespaces, ports, ingress
  host pattern, profile-specific values) flow through a `flywheel-config`
  ConfigMap in `flywheel-system`, populated from `flywheel.yaml` at
  `flywheel up` time.

### Three-tier scope model

Flywheel has three scope tiers; v0.1 ships the first two.

| Tier | What | Client opts in via |
|------|------|---------------------|
| **Core (always)** | dev-loop, traefik, sops, Flux entrypoint, namespaces | implicit — you get Flywheel |
| **Local-profile (pick one)** | `mkcert` (default) OR `tailscale-le-wildcard` | `flywheel.yaml: local.profile: <name>` |
| **Optional components (pick any)** *— v0.2+* | CNPG, prometheus-stack, MinIO, registry-GC, prod-stub-aws, gpu-nvidia, ... | `flywheel.yaml: components: [cnpg, prometheus-stack]` |

The "always-on" Flywheel infra is small in absolute terms (~8 files). The bulk
of infra across the data-platform client/self-hosted client is *opt-in modular operators*; we hold those for
v0.2 once we have evidence from running v0.1 in production.

## Flywheel repo layout (`cobr-io/flywheel`)

```
flywheel/
├── README.md
├── go.mod / go.sum
├── cmd/
│   ├── flywheel/             # the Go CLI binary
│   └── image-builder-controller/   # the in-cluster controller binary
├── internal/
│   ├── controller/                 # controller-runtime reconciler
│   │   ├── gitrepository_build_controller.go
│   │   └── templates/kaniko-job.yaml   # //go:embed
│   └── cli/                        # new / up / down / update / add-app / doctor / clean
├── Dockerfile.git-server
├── Dockerfile.git-auto-sync
├── Dockerfile.image-builder-controller
├── scripts/
│   ├── git-server/entrypoint.sh
│   └── git-auto-sync/sync.sh
├── manifests/                      # what Flux consumes from Flywheel mirror
│   ├── dev-loop/
│   │   ├── base/                   # env-agnostic dev-loop manifests
│   │   │                           #   (incl. inotify-bump DaemonSet:
│   │   │                           #    one-shot privileged sysctl writer)
│   │   └── overlays/local/         # local-only patches
│   ├── infra/
│   │   ├── base/                   # env-agnostic infra (traefik base)
│   │   └── overlays/
│   │       ├── local-mkcert/       # local profile A
│   │       └── local-tailscale/    # local profile B (prod overlays land
│   │                               #   in v0.2+ when multi-env arrives)
│   └── per-app-template/           # source for `cp -r` into client repo (v0.1)
├── templates/client-skeleton/      # what `flywheel init` writes out
│   ├── README.md.tmpl
│   ├── flywheel.yaml.tmpl
│   ├── .gitignore
│   ├── .sops.yaml.tmpl
│   ├── apps/...
│   ├── builders/...
│   ├── infra/...
│   └── clusters/local/flux-system/...
├── docs/
│   ├── architecture.md
│   ├── adding-an-app.md
│   ├── local-profiles.md
│   └── migrating-from-fork.md
└── .github/workflows/
    ├── release.yml                 # goreleaser: binary + multi-arch images
    └── test.yml                    # k3d e2e + go test
```

Notes:

* `manifests/` is Kustomize-pure — no Go templating at this level. Parameters
  flow through `flywheel-config` ConfigMaps populated by the CLI.
* `templates/client-skeleton/` is what the Go CLI renders into a new client
  repo at `flywheel init` time. Has Go-template placeholders
  (`{{ .ClientName }}`, etc.).
* `manifests/per-app-template/` is shipped both as a reference (for v0.1
  manual copy) and as the source `flywheel add-app` uses.

## Client repo layout (after `flywheel init acme`)

```
acme-gitops/
├── README.md
├── flywheel.yaml                    # version pin + answers (see § flywheel.yaml)
├── .sops.yaml                      # generated, with the client's age public key
├── .gitignore
├── apps/
│   ├── base/kustomization.yaml     # empty `resources: []`
│   └── overlays/local/kustomization.yaml
├── builders/
│   ├── base/kustomization.yaml     # resources: [./_self]; per-app folders added later
│   └── overlays/
│       └── local/kustomization.yaml   # resources: [../../base]
├── infra/
│   └── overlays/
│       └── local/kustomization.yaml   # wraps flywheel//manifests/infra/overlays/local-<profile>
├── clusters/
│   └── local/
│       └── flux-system/
│           ├── kustomization.yaml
│           ├── flywheel-source.yaml         # GitRepository → in-cluster Flywheel mirror,
│           │                                #   pinned by commit SHA (CLI-resolved from tag)
│           ├── self-source.yaml             # GitRepository → in-cluster git-server (client repo)
│           ├── flywheel-config.yaml         # ConfigMap derived from flywheel.yaml
│           ├── apps-kustomization.yaml
│           ├── infra-kustomization.yaml
│           ├── builders-kustomization.yaml  # path: flywheel//manifests/dev-loop/overlays/local
│           ├── client-builders-kustomization.yaml  # path: ./builders/overlays/local
│           ├── namespaces.yaml
│           └── traefik-tls.yaml             # mkcert profile only
└── cert/                           # mkcert output (gitignored, mkcert profile only)
```

A fresh client repo is ~17 files, virtually all under 30 lines. The current
reference-gitops has ~50 files including the entire dev-loop machinery in source
form — that source disappears from the client side under 3b.

The age private key for the local SOPS profile is **not** committed. `flywheel
new` generates a fresh age keypair, writes the public key into `.sops.yaml`,
and writes the private key to `~/.config/flywheel/<client>/age.key` (0600,
host-only). `flywheel up` loads the key from there and materialises it as the
in-cluster Secret Flux's SOPS decryption consumes. The `prod/` profile (v0.2+)
uses its own separately-generated key, also never in the repo. v0.1 assumes
single-developer-per-client; multi-developer key sharing is an open issue for
v0.2+ (see § Open issues). The committed file never contains plaintext secret
material.

`prod/` directories are deliberately absent in v0.1. Real multi-env arrives
in v0.2+ via a `flywheel new-env prod` (or similar) subcommand; until then,
nothing under `clusters/prod/`, `apps/overlays/prod/`, `builders/overlays/prod/`,
or `infra/overlays/prod/` exists in a fresh skeleton.

No top-level `Makefile`. The CLI is the only entrypoint; if a client wants
project-specific tasks (DB migrations, fixture seeding), they add their own.

## Local profiles

A profile is selected at `flywheel init --profile=<name>` time and persisted
to `flywheel.yaml`. **Switching is declarative**: edit `flywheel.yaml: local.profile`
and re-run `flywheel up`. The reconcile diff in step 4 of `up` detects the
profile change and prints the destructive set (e.g. mkcert→tailscale removes
no Flywheel-managed HelmReleases but the inverse removes cert-manager,
tailscale-operator, their HelmReleases, and any LE wildcard `Certificate`s)
and requires `--yes` before deleting. Two classes of resources are tiered
out of auto-deletion even with `--yes`:

* **PVCs** — `up` reports them as orphaned; user clears them with
  `flywheel clean` after confirming nothing else cares.
* **CRDs** — never auto-deleted, because dropping a CRD cascade-deletes every
  CR including any the user added directly. `up` reports orphaned CRDs and
  requires `flywheel clean --crds` to remove. Before deleting a CRD,
  `clean --crds` scans for CRs not labeled
  `app.kubernetes.io/managed-by=flywheel` and aborts with a list if any are
  found, forcing the user to delete or relabel them first.

### Profile A: `mkcert` (default)

* Traefik install (HelmRelease pinned to a Flywheel-tested version).
* Local TLS via `mkcert`: `flywheel up` generates `cert/{cert,key}.pem`
  via `mkcert -install` + `mkcert localdev.me "*.localdev.me"`, then installs
  them as the `local-cert` secret.
* Traefik `TLSStore default` references that secret.
* Ingress class `traefik`.
* Documented dnsmasq + `/etc/resolver/localdev.me` setup for `*.localdev.me`
  resolution.

Client provides: nothing. Works on Mac/Linux with `mkcert` installed.

### Profile B: `tailscale-le-wildcard` (advanced)

From the self-hosted client's pattern:

* Traefik install.
* **Tailscale operator** install (HelmRelease) — provisions a single tailnet
  proxy pod (`ts-traefik`) fronting Traefik, accessible from any device on the
  tailnet.
* **cert-manager** install + ClusterIssuer (Let's Encrypt DNS-01).
* Wildcard `Certificate` for `*.<client>.cobr.io` (or similar).
* Real DNS via the tailnet; no per-machine `mkcert -install` needed.

Client provides:

* Tailscale OAuth client secret (SOPS-encrypted in
  `clusters/local/flux-system/tailscale-oauth.enc.yaml`).
* DNS provider credentials for cert-manager DNS-01 (SOPS-encrypted).
* `local.tailscale.wildcard` and `local.tailscale.dns_provider` in
  `flywheel.yaml`.

Both profiles ship in v0.1; `mkcert` is the default because it has zero
prerequisites beyond a `brew install mkcert`.

## `flywheel.yaml` schema

The single source of truth for client-specific parameters. Read by the Go CLI
and materialised into a `flywheel-config` ConfigMap in the cluster.

Two files: `flywheel.yaml` (committed, shared across developers) and
`flywheel.yaml.local` (gitignored, host-specific overrides). The CLI deep-merges
`.local` over the committed file; `.local` values always win. Map keys merge
recursively; **arrays are replaced wholesale** — if `.local` declares any
list-valued field, that list fully supersedes the committed list (no concat,
no merge-by-index, no merge-by-key). The committed file MUST NOT contain
absolute host paths or per-developer values.

```yaml
# flywheel.yaml — committed
schema: v1alpha1                          # see § schema versioning below

flywheel:
  version: v0.1.0                         # label of the Flywheel release this
                                          # client was scaffolded against; the
                                          # CLI binary's embedded build SHA is
                                          # what actually pins Flux's
                                          # flywheel-source GitRepository
  # images:                               # OPTIONAL per-image overrides; omit
  #                                       # for the public ghcr.io defaults.
  #                                       # Override in flywheel.yaml.local
  #                                       # for per-developer dogfood builds.
  #   git-server: my-local/git-server:dev
  #   git-auto-sync: my-local/git-auto-sync:dev
  #   image-builder-controller: my-local/image-builder-controller:dev

client:
  name: acme
  org: cobr-io                            # GitHub org (optional, hint only)

cluster:
  name: acme-local
  registry: acme-local-registry
  registry_port: 50001                    # base port; allocator picks per host
                                          # (5000 avoided: macOS AirPlay default)
  http_port: 8083                         # allocated by `flywheel init` from a
  https_port: 8543                        # pool; recorded in
                                          # ~/.config/flywheel/allocations.json
  servers: 1
  agents: 2
  k3s_image: v1.34.1-k3s1

namespaces:
  flywheel: flywheel-system               # where Flywheel components install
  apps: apps

flux:
  interval_local: 10s
  iac_interval: 30s

local:
  profile: mkcert                         # mkcert | tailscale
  domain: localdev.me                     # mkcert profile only
  # tailscale:
  #   wildcard: "*.acme.cobr.io"
  #   tailnet: acme.ts.net
  #   dns_provider: route53

sops:
  age_recipients_local:
    - age1qx5vhc3z8q...
```

```yaml
# flywheel.yaml.local — gitignored, per-developer
paths:
  workspaces_root: /Users/dev/src/github.com/cobr-io  # host dir mounted to
                                                           # /workspaces inside
                                                           # the cluster

# Optional per-developer port overrides if the global allocations.json
# conflicts with something else on this host:
# cluster:
#   http_port: 9083
#   https_port: 9543
```

`paths.workspaces_root` lives only in `flywheel.yaml.local` — the committed
file never contains absolute host paths. On first `flywheel up`, the CLI
auto-detects from the repo's parent directory and writes `.local` if missing.
The skeleton's `.gitignore` includes `flywheel.yaml.local`.

### Port allocation

Per-client ports (`registry_port`, `http_port`, `https_port`) are allocated at
`flywheel init` time from documented pools, recorded in
`~/.config/flywheel/allocations.json` (host-global, not per-repo):

| Resource | Pool | Notes |
|----------|------|-------|
| `registry_port` | 50001-50099 | Skip 5000 (macOS AirPlay Receiver) and 50000 (some k3d defaults) |
| `http_port`     | 8080-8099   |  |
| `https_port`    | 8540-8559   |  |

`flywheel init` picks the next free triple, records it, and writes the values
into `flywheel.yaml`. `flywheel doctor` validates no live collision (e.g. a
non-Flywheel process took the port). Allocator entries are removed on
`flywheel destroy`. If a user `rm -rf`s a client repo without running
`destroy`, the entry leaks; `flywheel allocator gc` scans every recorded entry,
checks whether the recorded repo path still exists, and prunes orphans.
Pruning is explicit (not auto-run on `doctor` or `up`) because the pool is
large enough that leaks don't matter for years.

### Image resolution

The three Flywheel container images (`git-server`, `git-auto-sync`,
`image-builder-controller`) are the **one genuinely irreducible external
dependency** — they have to come from somewhere. Everything else (skeleton,
manifests, per-app template) is `go:embed`'d into the binary.

For each image, `flywheel up` resolves the reference in this order:

1. **Override from `flywheel.images.<name>`** in the merged
   `flywheel.yaml` (`.local` wins per the existing config-merge rules) —
   `.local` is the natural home for per-developer dogfood overrides.
2. **Default**: `ghcr.io/cobr-io/<name>:<flywheel.version>`.

Once resolved, the reference is used in two places: the kustomize image
transform applied during the bootstrap dance (step 11a), and Flux's
`spec.images` rewrite on the `flywheel-dev-loop` Kustomization (so
subsequent reconciles converge on the same refs).

**How the image reaches the cluster** depends on what the ref looks like:

> **Superseded (2026-06-17, see CHANGELOG):** `flywheel up` no longer
> `k3d image import`s any image. Every image — released or dogfood — is now
> mirrored into the cluster's local registry and referenced by its registry
> pull ref, so all nodes pull on demand (issue #14). The k3d-import dogfood
> path described below is historical.

* **Ref present in the host's local docker store** (detected via
  `docker inspect`): after the k3d cluster is created, `flywheel up`
  runs `k3d image import <ref> -c <cluster>` to load it directly into
  the cluster's containerd. No registry round-trip. This is the dogfood
  path: `docker build -t flywheel-dev/git-server:latest …`, point
  `flywheel.yaml.local: flywheel.images.git-server` at it, run `up`.
* **Ref not in local docker** (e.g. the default `ghcr.io/cobr-io/…`):
  the existing `Mirror` step pulls + retags + pushes to the per-client
  k3d registry.

**Option (c) — explicit failure on the gap.** If an image's resolved ref
is the default `ghcr.io/cobr-io/<name>:<version>` AND the pull fails
(no published release for that version), `up` exits with a message
that names the missing image and shows the exact `flywheel.images`
override stanza to add. No silent fallbacks, no auto-build-from-source
heroics — the failure points the user at the right knob.

### Schema versioning

The `schema:` field versions the *shape* of `flywheel.yaml` and is independent
of `flywheel.version`:

* During v0.x, the schema label is `v1alpha1`. Per § Versioning, v0.x makes no
  compat promise: the *shape* under `v1alpha1` may change between v0.x minor
  releases. The CLI's auto-migration still runs (so going from v0.1 → v0.4 is
  one command), but no stability is guaranteed. Graduates to `v1` at the v1.0
  release, after which the field bumps only on breaking shape changes.
* `schema:` bumps only on breaking changes to the file shape (rename, removal,
  required-field addition). Most flywheel minor releases keep the schema.
* On read, the CLI compares `schema:` against its native schema and either
  passes through (equal), auto-migrates (older, migration available), or
  refuses to run (newer than CLI knows). Migrations are embedded in the CLI
  binary; see § Versioning and compatibility.

### The `flywheel-config` ConfigMap

A flattened subset of `flywheel.yaml`, written to `flywheel-system/flywheel-config`
by `flywheel up`. Pods reference it via `envFrom` or `valueFrom`.

| Key | Source field | Consumed by |
|-----|--------------|-------------|
| `client.name` | `client.name` | git-auto-sync (commit author identity), image-builder-controller (label prefix) |
| `cluster.name` | `cluster.name` | image-builder-controller (registry URL construction) |
| `cluster.registry` | `cluster.registry` | image-builder-controller, kaniko Job templates |
| `cluster.registry_port` | `cluster.registry_port` | image-builder-controller |
| `namespaces.flywheel` | `namespaces.flywheel` | every Flywheel pod (own namespace ref) |
| `namespaces.apps` | `namespaces.apps` | image-builder-controller (where to look for ImageRepositories) |
| `flux.interval_local` | `flux.interval_local` | rendered into GitRepository / Kustomization specs |
| `local.profile` | `local.profile` | up-time profile branching; not read at runtime |
| `local.domain` | `local.domain` (mkcert) | git-server (ingress host construction) |

Nothing under `paths.*` or `sops.*` lands in the ConfigMap — those are
host-only or secret-only.

Notes:

* Bumping `flywheel.version` is the only common edit. Everything else is set
  once at `new` time.

## CLI surface

| Command | Purpose |
|---------|---------|
| `flywheel init [<path>]` | Scaffold a client repo. No arg: initialise cwd (client name = cwd basename). With arg: create `<path>` (or use it if empty) and initialise. Refuses any non-empty target dir (only `.git/` allowed). |
| `flywheel up [--yes] [--interactive]` | Reconcile cluster to `flywheel.yaml` |
| `flywheel down` | Stop the k3d cluster (preserves state) |
| `flywheel destroy` | Delete cluster + registry (destructive, requires confirm) |
| `flywheel update [--to=vX.Y.Z] [--apply]` | Bump `flywheel.version` + 3-way-merge skeleton; `--apply` directly applies dev-loop manifests for rollback when the dev loop is broken |
| `flywheel add-app <name>` | Scaffold a per-app builder folder |
| `flywheel doctor` | Check host prerequisites, profile-specific requirements |
| `flywheel clean [--profile=<name>] [--orphaned] [--crds]` | Opt-in destructive cleanup: profile-specific artifacts (cert/, old Secrets), state-bearing resources `up` reported as orphaned (PVCs), and/or orphaned CRDs (`--crds`; refuses if foreign CRs remain) |
| `flywheel snapshot [--out=<path>]` | Dump a canonicalised yaml view of cluster state (sorted map keys, runtime-only fields stripped; list order preserved as-emitted) for migration diffing without `dyff`/`yq` |
| `flywheel allocator gc` | Prune entries in `~/.config/flywheel/allocations.json` whose repo path no longer exists (leaks from `rm -rf` without `destroy`) |

`tls-config`, `tls-install`, `build-bins`, `flux-install` from the current
reference-client `Makefile` **all disappear** as user-facing commands. They become
internal phases of `up`.

### `flywheel init [<path>] [--profile=mkcert|tailscale] [--org=<gh-org>]`

Cwd-or-path semantics:
* **`flywheel init`** (no arg) — initialise the *current* working directory.
  Client name defaults to the cwd's basename. Conventional flow:
  `mkdir my-gitops && cd my-gitops && flywheel init`.
* **`flywheel init <path>`** — create `<path>` if it doesn't exist (or
  reuse it if it does), and initialise that directory. Client name
  defaults to `<path>`'s basename.

Either way, the target dir MUST be empty or contain only `.git/` —
refuses anything else, so an accidental `init` over real content fails
loudly.

```
1. Resolve target = cwd (no arg) or <path> (with arg); mkdir if missing.
   Refuse if target has content other than .git/.
2. cd target (no-op when target == cwd).
3. git init (idempotent — skips when .git/ exists).
4. Allocate ports from ~/.config/flywheel/allocations.json (registry/http/https)
   and record under client name, including the absolute repo path so
   `flywheel allocator gc` can prune the entry later if the repo is removed.
5. Render the embedded client-skeleton (go:embed templates/client-skeleton/)
   → ./ using values:
     ClientName=<basename>, Profile=<profile>, Org=<org>,
     ports from step 4, FlywheelSHA=<embedded build SHA>.
   No git clone of github.com/cobr-io/flywheel needed: the skeleton ships
   inside the binary.
6. Generate a fresh age keypair. Write the public key into .sops.yaml
   (creation_rules.age). Write the private key to
   ~/.config/flywheel/<client>/age.key with mode 0600. NOTHING SECRET IS
   WRITTEN TO THE REPO: the key lives only on the developer's host, never
   appears in git history, never trips GitHub secret scanning, and is not
   recoverable from a repo leak. `flywheel up` reads the key from ~/.config
   and materialises it as the in-cluster Secret Flux's SOPS decryption
   consumes. v0.1 assumes single-developer-per-client; multi-developer key
   sharing (either pass the existing key out-of-band, or add per-developer
   public keys to `.sops.yaml: creation_rules.age` and re-encrypt) is an
   open issue for v0.2+ via a future `flywheel age add-recipient` subcommand.
   The `prod/` profile (v0.2+) uses a separately-generated key, also never
   in the repo.
7. Write .flywheel-state.yaml:
     { flywheel_sha: <embedded build SHA>,
       answers: <snapshot of flywheel.yaml at render time>,
       files: { <relpath>: <sha256>, ... } }
   The `answers:` snapshot lets `flywheel update` re-render the base side of
   its 3-way merge with the original answers even if the user later edits
   `flywheel.yaml` answer values (rename, port bump, etc.); without it, the
   merge would misattribute answer changes as upstream changes.
8. Write flywheel.yaml.local with auto-detected paths.workspaces_root; add it
   to .gitignore.
9. git add -A && git commit -m "chore: bootstrap from flywheel <version>"
10. Print next steps (the appropriate one of `flywheel up` or
    `cd <path> && flywheel up`).
```

`init` is **fully offline**: it never touches the network. The skeleton,
manifests, and per-app template are embedded in the binary; the only thing
remotely sourced (and only by `up`, not `init`) is the three container
images — and only if `flywheel.images` overrides aren't set.

### `flywheel up`

The universal reconciliation command. Diff-applies whatever `flywheel.yaml`
declares against current cluster state.

```
 1. Read flywheel.yaml (and flywheel.yaml.local if present), validate against
    schema (CLI knows v1alpha1 during v0.x, will graduate to v1 at v1.0).
    Auto-migrate from older schemas if needed.
 2. flywheel doctor --quick (checks ONLY required OS deps: git, k3d, docker
    CLI + daemon; plus mkcert if profile=mkcert. kubectl/flux/sops/kustomize
    are NOT checked because Flywheel embeds them — see § Prerequisites).
    Runs *before* any network call so missing deps fail in sub-second.
 3. Ensure local Flywheel clone at the pinned version using embedded go-git
    (no host `git` shell-out): ~/.cache/flywheel/<version>/ ← shallow clone of
    https://github.com/cobr-io/flywheel at tag <version>. Resolve the tag to
    a commit SHA via go-git ls-remote (or read from the existing cache); the
    SHA is what Flux pins against — the tag is the human-readable label in
    flywheel.yaml. Idempotent: skipped if directory already present and HEAD
    matches the recorded SHA.
 4. Compute reconcile diff: walk desired state (rendered from flywheel.yaml +
    cached manifests) vs. actual cluster state. Categorise into
    +additive / ~mutating / -destructive. If anything is destructive (e.g.
    profile switch removes cert-manager HelmRelease), print the diff in
    `terraform plan`-style and require --yes (or interactive confirm) before
    continuing. --yes-additive auto-approves the +additive subset only. CRDs
    and PVCs are *never* in the destructive set even when their parent
    HelmRelease goes; they're surfaced as orphaned for `flywheel clean`.
 5. Load the age private key from ~/.config/flywheel/<client>/age.key
    (mode-check 0600); abort with a clear error if missing. Hold in memory;
    written to the cluster as a Secret in step 13. mkcert profile additionally:
    `mkcert -install` + generate cert/{cert,key}.pem if not already present.
 6. Create k3d registry <registry> on host port <registry_port> (idempotent).
 7. Create k3d cluster <cluster-name> if it doesn't exist with:
      - server/agent count from flywheel.yaml
      - registry-use <registry>
      - host port mappings <http_port>:80 + <https_port>:443 on loadbalancer
      - volume mount <workspaces_root>:/workspaces (server + agents)
 8. (inotify limits are raised by a privileged one-shot DaemonSet shipped in
     manifests/dev-loop/overlays/local — see step 11a — so no host-side
     sysctl step is needed here.)
 9. Mirror Flywheel images from ghcr.io to the local registry:
      for img in git-server git-auto-sync image-builder-controller; do
        docker pull ghcr.io/cobr-io/$img:<version>
        docker tag  ghcr.io/cobr-io/$img:<version> \
                    localhost:<registry_port>/$img:<version>
        docker push localhost:<registry_port>/$img:<version>
      done
10. Install Flux: apply embedded Flux install manifests (image-reflector +
    image-automation included) via in-process client-go. No `flux` CLI invoked.
    All client-go applies in this step and below use server-side-apply with
    fieldManager="flux-controller" so subsequent Flux reconciles silently
    adopt ownership without conflict warnings or drift-restomp loops.
11. Bootstrap dance (resolves the chicken-and-egg between Flux needing
    git-server and git-server living in the Flywheel mirror):
      a. Apply ~/.cache/flywheel/<version>/manifests/dev-loop/overlays/local
         via embedded kustomize build → client-go server-side-apply
         (fieldManager "flux-controller"). Brings up git-server +
         image-builder-controller + image-update-automation, plus a one-shot
         privileged DaemonSet that runs `sysctl -w fs.inotify.max_user_*`
         on each node and exits. Uses the cached local clone, so no runtime
         ghcr.io / GitHub dependency.
      b. Wait for git-server Deployment Ready (client-go watch).
      c. Push the cached Flywheel clone into the in-cluster git-server as a
         second bare repo `flywheel.git`, tagged at the commit SHA recorded
         in step 3. This is what `flywheel-source.yaml` resolves against — at
         no point does Flux reach github.com/cobr-io/flywheel at runtime.
         Idempotent: skipped if the in-cluster remote is already at the
         expected SHA. Also push the client's own working tree into its bare
         repo (same as today's bootstrap).
      d. Apply clusters/local/flux-system/ (same kustomize+client-go path,
         same fieldManager). Flux takes over, reconciling both
         GitRepositories (Flywheel mirror + self) into the cluster.
         Subsequent reconciles flow through Flux.
12. If destructive ops were approved in step 4: apply the deletions via
    client-go delete (orphaned HelmReleases / Certificates / etc.).
    PVCs and CRDs are *never* auto-deleted; they're reported as orphaned and
    require `flywheel clean` (PVCs) or `flywheel clean --crds` (CRDs;
    refuses if any CR is not labeled
    `app.kubernetes.io/managed-by=flywheel`).
13. Create the age private-key Secret in flux-system from the in-memory key
    loaded in step 5. If profile=mkcert: also create the `local-cert` TLS
    Secret in `kube-system` via client-go. If profile=tailscale: generate
    SOPS secret stubs if missing, abort with instructions to fill them in.
14. Wait for Flux to report all Kustomizations Ready (client-go watch with a
    timeout, optionally --wait=false).
15. Print: "Cluster up. Visit https://hello.<domain> once you've added an app."
```

Idempotent: re-running `up` on an unchanged repo is a no-op modulo a Flux
poll. A profile switch (edit `flywheel.yaml: local.profile: tailscale`,
re-run `up`) detects the change in step 4, prints the destructive diff
(removes cert-manager HelmRelease? leaves an orphaned tailscale operator?),
requires `--yes`, then applies. Total time on a warm Mac with docker
pre-running: ~60-90s; cold colima start adds ~30s.

PVCs and CRDs are tiered out of auto-deletion: even with `--yes`, `up`
reports them as orphaned and asks the user to run `flywheel clean` or
`flywheel clean --crds` explicitly. Rationale: dropping a PVC with real
data, or cascade-deleting CRs the user added directly to a CRD, is not
recoverable.

### `flywheel update [--to=vX.Y.Z] [--apply]`

```
1. Read flywheel.yaml current version + .flywheel-state.yaml (last render
   `flywheel_sha`, the `answers:` snapshot used at that render, and per-file
   source hashes).
2. Resolve target version: tag (default: latest release tag from
   cobr-io/flywheel) → commit SHA via go-git ls-remote. Populate
   ~/.cache/flywheel/<target>/ if not already cached.
3. Show release notes diff between current and target.
4. Update flywheel.yaml: flywheel.version = <target tag> (human-readable).
   The SHA is written into Flux's flywheel-source.yaml in step 7.
5. 3-way merge the client-skeleton:
     base    = templates rendered at `.flywheel-state.yaml.flywheel_sha`
               using `.flywheel-state.yaml.answers:` (the snapshot of
               flywheel.yaml at the *previous* render — NOT the current
               flywheel.yaml — so any answer the user edited since
               `new`/`update` doesn't look like an upstream change)
     ours    = working tree
     theirs  = templates rendered at <target SHA> using *current*
               flywheel.yaml answers (so a port renumber or rename flows
               in cleanly through the `theirs` side)
   Files unchanged from base render are overwritten silently. Files the user
   has edited and which also changed upstream get either a clean merge or
   `<<<<<<<` conflict markers; CLI prints which.
6. Write new .flywheel-state.yaml:
     flywheel_sha = <target SHA>
     answers      = snapshot of *current* flywheel.yaml (post-update)
     files        = refreshed sha256 of each rendered/merged file
   After conflict markers are resolved manually, run
   `flywheel update --refresh-hashes` to rewrite `files:` against the working
   tree without re-rendering — otherwise the next update will treat every
   conflict-resolved file as user-edited.
7. Update clusters/local/flux-system/flywheel-source.yaml:
   `spec.ref.commit = <target SHA>`.
8. git add + commit "chore: bump flywheel to <target tag>".
9. Default: stop. The new SHA pin is committed but the in-cluster Flywheel
   mirror is still at the previous SHA — Flux won't see the change until the
   mirror is repushed. Run `flywheel up` to do that (step 11c re-pushes the
   cache to the in-cluster bare repo at the new SHA, after which Flux
   reconciles on its next poll). If --apply is passed (rollback /
   "dev-loop is bricked" path), `update` also pushes the mirror and applies
   ~/.cache/flywheel/<target>/manifests/dev-loop/overlays/local via embedded
   kustomize + client-go, recovering the dev loop without depending on its
   own delivery.
10. Print: "Run `flywheel up` to push the new SHA into the in-cluster mirror.
    Use --apply if the dev loop is broken."
```

`update` does *not* touch the running cluster by default — it's a git-state
operation. Run `flywheel up` afterwards to push the new Flywheel commit SHA
into the in-cluster mirror, after which Flux reconciles on its next poll.
Verify with `kubectl get gitrepository flywheel -n flux-system -o yaml`.

### `flywheel add-app <name>` (v0.1: thin)

In v0.1, this is `cp -r + sed` glorified:

```
1. Validate <name>.
2. Read the embedded per-app-template (go:embed manifests/per-app-template/).
   No cache, no clone, no go-git — the template ships inside the binary.
3. Render templates with --name, --image, --context, --dockerfile flags.
4. Write to builders/base/<name>/.
5. Append `  - ./<name>` to builders/base/kustomization.yaml.
6. Print: "Edit apps/base/<name>/deployment.yaml manually — Flywheel doesn't
   scaffold app manifests in v0.1."
```

v0.3 will add `update-app <name>` with copier-style 3-way merge per file. v0.1
emits files and walks away.

## Versioning and compatibility

Semver. The single version `vX.Y.Z` covers Flywheel atomically — the Flux
GitRepository tag *and* the three container image tags advance in lockstep.

**v0.x phase (first 6-12 months):**

* No compatibility promise between minor versions. Any release may move
  things, rename CLI flags, or restructure manifest paths. `flywheel.yaml`'s
  `schema: v1alpha1` label reflects this; auto-migrations exist, but
  stability does not. The label graduates to `schema: v1` at v1.0.
* `flywheel update` prints release notes between current and target.
* Releases tagged frequently — every few days during active development is
  fine.
* Client count in v0.x stays small: the reference client + the synthetic
  `flywheel-dogfood` from Phase 3. The self-hosted client waits on the v0.2+ catalog
  (CNPG, MinIO, GPU); the data-platform client waits on prod-stub + operators.

**v1.0.0 marks the compatibility commitment:**

* **Major bump (v2.0.0)** = breaking changes to `flywheel.yaml` schema,
  manifest paths consumed by Flux, container image CLI flags, or
  CLI command names/flags that scripts depend on.
* **Minor bump (v1.X.0)** = additive only: new optional fields in
  `flywheel.yaml`, new optional manifests, new CLI subcommands, new optional
  components.
* **Patch (v1.X.Y)** = bug fixes, security patches.
* Every release has a CHANGELOG.md entry (Keep-a-Changelog conventions).
* Each `v1.X.0` provides a deprecation window: anything removed in `v2.0` must
  have been deprecated with a warning in some `v1.X.0` first.

**Migration scripts.** Breaking changes ship a migration command:
```
flywheel migrate --from=v1.X --to=v2.0
```
Rewrites `flywheel.yaml` and client-side files to fit the new schema. Pattern
modelled on copier's `_migrations` hooks: per-version scripts embedded in the
CLI binary. The `schema:` field in `flywheel.yaml` lets the CLI detect mismatch
and either auto-migrate or refuse to run.

**Tooling.** `goreleaser` for binary + multi-arch container images; a single
`git tag vX.Y.Z && git push --tags` triggers the full release pipeline. GitHub
Releases auto-generated with the changelog excerpt. Images on ghcr.io with
`:vX.Y.Z` tags only — no `:latest`, to force `flywheel.yaml` to be the source
of truth.

## Migration plan

### Phase 0 — Flywheel scaffold (week 1)

In `cobr-io/flywheel` (new repo):

* `go mod init`, lay out `cmd/`, `internal/`, `manifests/`, `templates/`,
  `scripts/`.
* Copy `Dockerfile.{git-server,git-auto-sync,image-builder-controller}` from
  reference-gitops. Strip reference-client literals.
* Copy `scripts/git-server/entrypoint.sh`, `scripts/git-auto-sync/sync.sh`, and
  the Go controller (`scripts/image-builder-controller/`) → `cmd/`. Update
  `go.mod`.
* Stub `manifests/dev-loop/base/` from the reference client's `builders/base/_shared/`.
  Parameterise registry name + namespace via env-var-from-ConfigMap. **All
  literal `reference-` strings become parameters.** Add the one-shot
  privileged `inotify-bump` DaemonSet here (writes `fs.inotify.max_user_*`
  via `sysctl -w` on each node and exits), replacing the host-side sysctl
  step the reference client relied on.
* Stub `manifests/infra/overlays/local-mkcert/` from the reference client's traefik-tls +
  namespace setup.
* Stub `manifests/infra/overlays/local-tailscale/` from the self-hosted client's Tailscale
  operator + cert-manager + LE wildcard pattern.
* Skeleton CLI: `new`, `up`, `down`, `doctor`. Other commands stubbed.

Deliverable: `goreleaser build` produces a binary; manifests apply cleanly to
a hand-rolled k3d cluster.

### Phase 1 — in-isolation v0.1.0 (week 2)

* `flywheel init test-client` writes a working skeleton.
* `flywheel up` brings up `test-client-local` k3d cluster end-to-end on
  a clean Mac.
* **Dev-loop validation** — must pass before tagging v0.1.0:
  1. **Baseline commit on main.** Edit code on sibling app repo `main`,
     commit. Pod runs new image within 60s.
  2. **App-repo branch switch.** `git checkout -b feat/foo` in the app repo,
     commit. App's `GitRepository.spec.ref.branch` gets patched to `feat/foo`;
     cluster reconciles to feature-branch commit; switch back to `main`,
     cluster reconciles back.
  3. **Client-gitops branch switch.** `git checkout -b experiment/raise-replicas`
     in the client gitops repo, edit `apps/base/<app>/deployment.yaml`, commit.
     Client gitops `GitRepository.spec.ref.branch` patches to the feature
     branch; Flux reconciles from it; replica count changes on cluster; switch
     back, reconciles back.
  4. **Both repos on independent feature branches simultaneously.** App on
     `feat/foo`, gitops on `experiment/raise-replicas`. Both reconcile
     independently. Switch each back independently. Exercises the decoupling
     of the two git-auto-sync instances.
* Both profiles validated: `mkcert` against `*.localdev.me`; `tailscale`
  against a real tailnet.
* CI: GitHub Actions runs `k3d` in a runner, executes the four scenarios
  above with the mkcert profile (tailscale skipped in CI, validated manually
  before each release).
* Tag `v0.1.0` from `main`; release pipeline runs.

Deliverable: `flywheel@v0.1.0` installable via `go install`, has a
working `new + up` flow, and the dev loop survives branch switches on both
repos independently.

### Phase 2 — reference-client migration (week 3)

In `example-org/reference-gitops`:

* **Capture a known-good baseline** before deleting anything. Use
  `flywheel snapshot --out=/tmp/reference-pre.yaml` (an internal helper that
  walks the cluster via client-go and writes a canonicalised yaml dump — map
  keys sorted, runtime-only fields like resourceVersion/managedFields
  stripped, **list order preserved** as the API server emits it). After
  migration, run `flywheel snapshot --out=/tmp/reference-post.yaml` and
  `git diff --no-index /tmp/reference-pre.yaml /tmp/reference-post.yaml`.
  Canonicalising map-key order is enough to make the diff readable without
  `dyff`/`yq`; list order is left alone because order-sensitive lists
  (containers, env, initContainers, args) would produce false "reorder"
  diffs that mask real ones. Any unexplained delta is a migration bug.
  Commit both snapshots to a scratch branch for the PR review.
* `git checkout -b flywheel-migration`.
* Delete `Dockerfile.*`, `scripts/git-server/`, `scripts/git-auto-sync/`,
  `scripts/image-builder-controller/`, `builders/base/_shared/`, `Makefile`.
  **~70% of file count goes.**
* Generate fresh `flywheel.yaml` and `clusters/local/flux-system/` from a
  `flywheel init` of an empty client, then layer the reference client's `apps/`,
  per-app builder folders, and overlays back in.
* Verify: `flywheel up` brings the cluster up; existing `hello-app`
  reconciles; commit-to-pod time matches today's ~25s; post-migration
  snapshot diffs cleanly against pre-migration baseline.
* Open PR for review. Merge.
* This run exercises Flywheel against a real client; expect 5-10 issues.
  Each fix lands as `v0.1.X`. The reference client's `flywheel.yaml` gets bumped repeatedly.

Deliverable: reference-gitops runs on `flywheel@v0.1.X` (some X); all
dev-loop functionality preserved; pre/post snapshot diff committed as
evidence.

### Phase 3 — stabilise (week 4)

* Documentation pass: `docs/architecture.md`, `docs/adding-an-app.md`,
  `docs/local-profiles.md`, `docs/migrating-from-fork.md`.
* Onboard a **synthetic `flywheel-dogfood` client** end-to-end through
  `flywheel init`. Throwaway repo with 2-3 toy apps deliberately chosen to
  exercise common patterns without committing a real customer to v0.x:
  a static HTML site (smallest possible builder), a tiny Go API
  (compiled-binary builder), and a Python worker (interpreter + deps
  builder). Surfaces issues that only show up on a true greenfield path —
  naming conventions, port allocation, mkcert flow on a never-before-seen
  host, the cache-populate-during-`new` step.
* Decision point: enough confidence to tag `v1.0.0`? If yes, commit to semver
  compat. If no, stay on v0.x and onboard one more (real) client first.

Deliverable: Flywheel is "shipped" — the reference client + one greenfield client both
running on stable releases.

### Phase 4 — self-hosted client + the data-platform client (future)

* **The self-hosted client** migrates next. Uses tailscale profile, exercises the riskier of
  the two profiles, tests GPU/storage parameterisation pressure points. Likely
  surfaces things added to the optional-components catalog (CNPG, registry-GC).
* **The data-platform client** waits for v0.2+ when the catalog includes prom-stack,
  cert-manager-prod-stub, AWS Load Balancer Controller, etc. The
  domain-specific operator stays in the data-platform client's repo forever — never Flywheel
  territory. Migration is its own 3-4 week project.

## Test strategy

**Three layers.**

* **Unit tests (Go).** Controller logic, CLI command parsing, `flywheel.yaml`
  schema validation, template rendering. Standard `go test ./...`. Cover
  everything in `internal/` with branching logic.

* **Integration tests (k3d in CI).** GitHub Actions workflow that:
  1. Installs k3d + mkcert in the runner (Docker is pre-installed on
     `ubuntu-latest`; git is pre-installed; flux/sops/kustomize/kubectl
     are embedded in the Flywheel binary and need no install step).
  2. Builds the binary.
  3. Runs `flywheel init test-client --profile=mkcert`.
  4. Runs `flywheel up`.
  5. Adds a sibling sample-app (committed under `testdata/sample-app/`),
     commits to `main`, asserts a pod comes up with the new image within 60s.
  6. Runs the three remaining branch-switch scenarios from Phase 1.
  7. Tears down with `flywheel destroy`.

  Runs on every PR; budget 10-15 minutes per run (Flux Kustomization +
  image-automation reconcile cycles dominate; 5-8 min is optimistic).
  The tailscale profile is harder to test in CI (real Tailscale credentials +
  DNS); skipped in CI, validated manually before each release.

* **Smoke tests on real clients.** After each `vX.Y.Z` tag, a script:
  1. Bumps reference-gitops `flywheel.yaml` to the new version.
  2. `flywheel up` on a fresh local cluster.
  3. Confirms the reference client's existing apps reconcile and a sample commit triggers
     a build.

  If smoke fails, the release rolls back (delete tag, fix forward). The reference client's
  pinned `flywheel.yaml` is not updated until smoke passes.

## Open issues / future work

1. **Per-app folders as Kustomize components** (3b-flavour from the
   brainstorm). v0.3+. Case strengthens once a client has 5+ apps.
2. **Optional-components catalog.** v0.2: CNPG, kube-prometheus-stack, MinIO,
   registry-GC, prod-stub-aws (cert-manager + AWS Load Balancer Controller +
   external-dns + IRSA), gpu-nvidia. Each is its own design conversation.
3. **`flywheel plan`.** Terraform-style dry-run. Useful as the diff
   surface grows.
4. **Multi-environment support.** v0.1 only scaffolds `clusters/local/`.
   A `flywheel new-env prod` subcommand plus real multi-env semantics
   (per-env `flywheel.yaml` overlays, or a separate `flywheel.<env>.yaml`)
   arrive in v0.2+ when the data-platform client migrates.
5. **Multi-tenant clusters.** Could a single k3d cluster host multiple
   clients side-by-side? Not in v0.1; one cluster per client is assumed.
6. **Registry GC.** Each build leaves a tag; weeks of dev fills disk. Borrow
   the self-hosted client's pattern as an optional component in v0.2.
7. **Air-gap mode.** `flywheel up --mirror-from=<internal-registry>`
   for v0.2+.
8. **CLI distribution channels.** v0.1: `go install` only. v0.2+: brew tap;
   possibly `curl | sh` installer.
9. **Migration of the data-platform client's `docker-builder` callers.** The data-platform client's existing
   `.docker-builder-config` files need a conversion script to Flywheel's
   `build-config.yaml` schema. Belongs in the data-platform client's migration project, not Flywheel
   core.
10. **Secrets-in-builds (`[[secrets]]`).** The data-platform client's docker-builder supports
    `--secret` mounts; the Kaniko-based Flywheel doesn't yet. Add when an app
    needs it.
11. **Branch switch with stale IAC commit in flight.** If the developer
    switches branches *while* IAC is pushing a tag bump to the bare repo,
    what happens? The rebase logic should handle it (the fix is in commit
    `5a999a9` already), but manual stress test before v1.0.
12. **Multi-developer age key sharing.** v0.1 generates a per-client age
    key on `flywheel init` and stores it host-only in `~/.config/flywheel/`.
    Teams with multiple developers currently must pass the key around
    out-of-band (1Password, etc.). v0.2+ should ship `flywheel age
    add-recipient <pubkey>` to register additional developers (appends to
    `.sops.yaml: creation_rules.age`, re-encrypts existing secrets to the
    new recipient set), and `flywheel age generate` for the joining
    developer's side.
