# Dev-loop latency: what to expect, and how to find a stall

The dev loop's speed is a product promise. This doc pins the expected numbers
(so a slowdown is recognized as a bug, not shrugged off as "Kubernetes being
Kubernetes"), attributes the budget to each hop, and shows how to measure.

All numbers measured 2026-07-12 on `main` (post PRs #74–#105), from scenario-1
timestamps across 14 CI runs (2026-07-03 → 2026-07-12) plus instrumented local
runs (M-series macOS + colima) with controller log followers. CI runners are
2-core `ubuntu-latest`; local is typically faster for builds, slower for
cluster bring-up.

## Headline expectations

| Path | Expected | Notes |
|------|----------|-------|
| Warm edit→served cycle (steady state) | **7–13 s** | includes the in-cluster image rebuild; THE number to protect |
| First deploy after `add app` (cold) | 11–28 s | first build has no layer cache; includes first-scan machinery |
| Any single warm leg (early bumps) | 7–13 s (was 7–41 s bimodal) | fixed: [#107](https://github.com/cobr-io/flywheel/issues/107) was a per-node cold pull of the buildkit client image from Docker Hub; `up` now pre-seeds it into the local registry |
| Gitops-repo edit → converged | ~2–12 s | git-deploy-controller tick 2 s + source fetch (poked) + apply (event-driven) |
| `flywheel up` (fresh cluster) | ~2–4 min local, ~9 min CI | cluster create + Flux install + bootstrap + image mirror |

Because #107 is a per-leg coin flip, judge loop health by the **fastest of
two consecutive warm bumps** (that's also how the CI gate works). If BOTH of
two consecutive warm cycles exceed ~20 s, treat it as a defect: either a poke
stopped firing (chain below) or a Flux object is in error/backoff.

## The warm-cycle budget, hop by hop

Measured from a fast-mode capture (commit `13:53:57Z` → new pod `13:54:09Z`,
12 s total):

| # | Hop | Budget | Mechanism (why it's fast) |
|---|-----|--------|---------------------------|
| 1 | commit → git-auto-sync push | ≤ 2 s | sidecar `POLL_INTERVAL=2s` |
| 2 | push → app GitRepository artifact | ≈ 0 s | sidecar pokes the GitRepository (`reconcile.fluxcd.io/requestedAt`) |
| 3 | artifact → build Job created | ≈ 1 s | image-builder-controller watches GitRepository (event) |
| 4 | build + push to local registry | 5–8 s warm | shared buildkitd daemon layer cache; cold builds add ~5–15 s. The per-Job CLIENT image is pre-seeded into the local registry by `up`, so a node's first build pulls it over the LAN (~2s), not Docker Hub (issue #107) |
| 5 | build container exit → ImageRepository scan | ≈ 0.2 s | buildjob-imagescan controller pokes the ImageRepository |
| 6 | scan → ImagePolicy resolves tag | ≈ 0.1 s | policy reconciles on ImageRepository status event |
| 7 | policy → IUA commits bump to deploy branch | ≤ 5 s | imagepolicy-iua controller pokes the IUA (interval 5 s is the fallback) |
| 8 | IUA push → self GitRepository artifact | ≈ 0 s | iua-source-poke controller pokes the source |
| 9 | artifact → kustomize apply | ≤ 2 s | kustomize-controller reconciles on source event; git-deploy-controller tick is 2 s |
| 10 | apply → new pod serving | 2–4 s | single-replica rollout of a small image |

Every "≈ 0" hop is a poke (`internal/fluxpoke`); every poke has an interval
fallback, so a dead poke does not break the loop — it quantizes the hop to its
interval (10 s for GitRepository/ImageRepository fetch/scan, 5 s for the IUA).
A loop that "works but feels slow" is almost always one or more dead pokes.

## How to measure

- **CI**: every k3d e2e run prints scenario-1's leg timestamps
  (`committed 'hello from sample-app v1'` → `served text matches: …`); the
  nightly `e2e-full` covers scenarios 2–4 as well.
- **Locally**: `make e2e` (or `scripts/e2e.sh`) runs the same scenario; for a
  one-off warm-leg measurement against a running cluster, commit to the app
  worktree and watch
  `kubectl -n apps get pods -w` — commit-to-new-pod is the number.
- **Attribution**: follow the controller logs during a bump —
  `image-builder-controller` (pokes 5+7 log as `poked …`),
  `image-reflector-controller` (scan results), `image-automation-controller`
  (IUA pushes), `kustomize-controller` (applies), `git-deploy-controller`
  (deploy-branch maintenance). A hop that waits out its interval instead of
  being poked names the culprit.

## Known deviations

- **[#107](https://github.com/cobr-io/flywheel/issues/107)** — early warm
  legs used to lose 15-30 s ~50% of the time: the build Job's thin buildkit
  CLIENT image was cold-pulled from Docker Hub once per NODE, and Jobs
  schedule on arbitrary nodes. Fixed by `up`'s mirror-buildkit-client step
  (LAN-registry pre-seed; Hub fallback when offline). Watch the CYCLE lines
  confirm the distribution collapses, then tighten the scenario-1 gate from
  min-of-two-legs to every-leg.
- **macOS + colima**: host-port forwarding can lag or wedge after rapid
  cluster create/delete cycles — `curl` against `*.localdev.me:<port>` failing
  while the pod is Running is a forwarding artifact, not a loop stall. Also
  [#106](https://github.com/cobr-io/flywheel/issues/106) (port-heal probe
  blind spot on macOS).

Keep this doc honest: if the budget changes (better or worse), update the
table in the same PR that changes the behavior, and re-run the measurement
rather than editing numbers from memory.
