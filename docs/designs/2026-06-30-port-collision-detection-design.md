# Design: cross-platform port-collision detection

**Status:** accepted
**Author:** Matthijs van der Kroon
**Date:** 2026-06-30

## Problem

`flywheel init` allocates a host-port triple (registry / http / https) and
`flywheel up`'s step-5b `portheal` re-probes those ports before k3d binds them,
reallocating any a foreign process now holds so `up` self-heals instead of
crashing k3d with "address already in use" (issue #1). Both paths decide
availability with `net.Listen` **on the host**:

- `netutil.PortIsBindable` — `net.Listen("127.0.0.1:p")` (`netutil.go:26`)
- `netutil.PortIsBindableWildcard` — `net.Listen("0.0.0.0:p")` (`netutil.go:37`)

That probe asks the wrong authority. The thing that decides whether a k3d
container can publish a host port is **docker's published-port ledger**, not the
host's bindability — and on macOS/colima those disagree, because docker runs in
a Lima VM and forwards published ports to the host. A host `net.Listen` then
succeeds even when docker already owns the port.

**Verified live (v0.1.0 pre-release E2E, colima):** flywheel's own probes
returned `bindable=true` for 50001 and 50002 while two existing k3d registries
held them; `flywheel init` handed out 50002, `up`'s portheal saw nothing to
heal, and `up` crashed at registry create (step 6) with:

```
Bind for 0.0.0.0:50002 failed: port is already allocated   (docker daemon)
```

So the issue-#1 self-heal is **non-functional on macOS/colima**. It works on
Linux/CI only because native docker shares the host network namespace, so a
published port really does occupy the host port and `net.Listen` catches it.
(See `docs/dev/dev-loop-validation.md` and the `colima-portheal-blind` memory.)

**Impact.** Any macOS/colima user with a second k3d cluster — or any container
publishing a port in flywheel's pools (`registry 50001–50099`, `http 8080–8099`,
`https 8540–8559`) — hits a hard `up` failure with no auto-heal. This is the most
common local setup, so it's a likely release blocker for the macOS happy path.

### In scope

- A port-availability check that is correct on macOS (colima **and** Docker
  Desktop), Linux (native docker), and Windows WSL2.
- Wiring it into three call sites: `init` (allocator), `up` (portheal), and
  `doctor`'s pre-flight port check (with the same "owned by this cluster"
  nuance, so it proactively warns instead of false-alarming on your own cluster).
- A defensive retry-on-collision net so a TOCTOU race or an unforeseen backend
  quirk degrades to a reallocation+retry rather than a crash.

### Out of scope

- Changing the pool ranges or the `allocations.json` ledger format.
- Reworking how k3d itself is invoked.
- A general docker SDK dependency (we keep shelling out, as the codebase does).

## Approach

Make availability consult docker's published ports as the **primary** signal and
keep the host bind probe as a **secondary** (to still catch non-docker host
squatters, e.g. a stray process on Linux/Docker Desktop):

```
available(port)  ==  NOT published-by-docker(port)  AND  host-bindable(port)
```

Why this is correct on every backend:

- **colima:** a docker-published port is host-bindable=true (the bug), but the
  `published-by-docker` term catches it. ✅
- **Linux / Docker Desktop:** a docker-published port fails the host bind anyway,
  and a non-docker squatter is caught by the host term. ✅

`docker ps --format '{{.Ports}}'` reports published host ports correctly under
colima (verified — it's how the collision was found), and the docker CLI behaves
the same across backends, so this is the cross-platform unifier.

### 1. Docker published-port query

A small helper queries docker once and returns the set of host ports any running
container publishes:

```go
// internal/cli/dockerports (new package — chosen over extending
// internal/cli/k3d so a docker-generic query isn't filed under a k3d pkg).
//
// PublishedPorts returns the set of host TCP ports currently published by any
// running docker container, parsed from `docker ps --format '{{.Ports}}'`.
// Best-effort: callers fall back to the host probe on error.
func PublishedPorts(ctx context.Context) (map[int]struct{}, error)
```

Parsing handles every `docker ps` port form:

```
0.0.0.0:50002->5000/tcp, [::]:50002->5000/tcp     → 50002
127.0.0.1:8080->80/tcp                            → 8080
0.0.0.0:8000-8002->8000-8002/tcp                  → 8000,8001,8002 (ranges)
5000/tcp                                           → (no host port; ignored)
0.0.0.0:53->53/udp                                 → udp ignored (k3d publishes tcp)
```

We collect the **host** port (left of `->`), across all bind addresses, tcp
only. udp and container-only ports are ignored (k3d's registry/serverlb/api
publishes are tcp). Ranges are expanded.

### 2. Composed probe, fetched once per pass

Both allocation paths build one probe that closes over a single docker snapshot:

```go
func dockerAwareProbe(ctx context.Context, out io.Writer) func(int) bool {
    published, err := dockerports.PublishedPorts(ctx)
    if err != nil {
        style.Warn(out, "could not read docker published ports (%v); "+
            "falling back to host-only port probe", err)
        published = nil // empty set → behaves like today
    }
    return func(port int) bool {
        if _, taken := published[port]; taken {
            return false
        }
        return netutil.PortIsBindableWildcard(port)
    }
}
```

One `docker ps` per `init`/`up` invocation — negligible cost. The wildcard host
probe (`0.0.0.0`) is the faithful secondary on every backend; we drop the
loopback variant for allocation decisions (it's the least faithful — see
`netutil.go` caveats).

### 3. Wiring

The allocation call sites already take an injected `bindable func(int) bool`, so
this is mostly a thread-the-probe change, not a rewrite:

| Path | Today | Change |
|---|---|---|
| `init` (allocator) | `allocator.go:162` package var `portIsBindable`; `pickFree` (`:183`) → `PickFreePort` (`:194`) | `init` builds the composed probe and passes it into `Allocate`; `Allocate` forwards it to `pickFree`. |
| `up` (portheal) | `portheal.go:77` passes `netutil.PortIsBindableWildcard` | pass the composed probe instead. |
| `doctor` (pre-flight) | `doctor/full.go:155` `portIsBindable` wraps `netutil.PortIsBindable` | use the composed probe, plus the `RegistryExists`/`ClusterRunning` "owned" guard so this cluster's own ports aren't flagged. |

`PickFreePort(pool, taken, bindable)` is unchanged — it already accepts the
probe. The only signature change is `allocator.Allocate` (see below).

### 4. Retry-on-collision net (defensive)

Even with a correct pre-check, a TOCTOU race (a port taken between probe and
bind) or an unforeseen backend quirk can still make k3d fail. So `up` wraps the
registry (step 6) and cluster (step 7) creates: on a docker port-allocation
error, re-run the heal for the affected slot and retry **once**.

```go
func createWithPortRetry(ctx context.Context, slot string, out io.Writer,
    create func() error, reheal func() error) error {
    err := create()
    if err == nil || !isPortAllocatedErr(err) {
        return err
    }
    style.Warn(out, "%s: port taken at create time; reallocating + retrying", slot)
    if rerr := reheal(); rerr != nil {
        return rerr
    }
    return create()
}

// Matches docker DAEMON messages (identical across client OSes), not client
// specifics: "port is already allocated" (publish conflict) and
// "address already in use" (bind conflict).
func isPortAllocatedErr(err error) bool { ... }
```

This is a backstop, not the primary mechanism — the docker-ledger pre-check
should make it rare.

## Platform compatibility

This is a hard requirement: the fix must be correct on macOS (colima + Docker
Desktop), Linux, and Windows WSL2. flywheel runs only inside WSL2 on Windows
(no native Windows build — see `docs/guides/windows-wsl.md`).

| Backend | Where docker runs | Host `net.Listen` reliability | Why this design is correct |
|---|---|---|---|
| **Linux, native docker** (CI) | host net namespace | Reliable — a published port occupies the host port | docker-ledger AND host probe both catch it; behavior unchanged from today |
| **macOS, colima** | Lima VM, forwarded | **Unreliable** — host bind succeeds despite docker holding it | docker-ledger term is authoritative; this is the fix |
| **macOS, Docker Desktop** | LinuxKit VM, vpnkit forward | Partial — vpnkit usually binds the host port, but semantics vary | docker-ledger term makes it deterministic regardless of vpnkit |
| **Windows WSL2** | Docker Desktop WSL integration or in-distro docker; flywheel runs in the WSL2 distro | Varies by integration mode | `docker ps` works in the distro; docker-ledger term is authoritative |

The unifier is that `docker ps` speaks to the **docker daemon** and reports the
same published-port ledger on every backend, independent of how (or whether)
those ports are forwarded to a host. The host `net.Listen` probe is kept only as
a secondary to catch *non-docker* host squatters, where its reliability is good
(it's the same OS binding the port).

Cross-platform implementation notes:

- **No OS-specific syscalls.** We shell out to `docker` (already a hard
  dependency, checked by `doctor`); docker abstracts the platform.
- **Retry-net error matching keys on docker daemon strings** (`port is already
  allocated`, `address already in use`) — produced by the daemon, identical
  across client OSes — never on client-OS error text.
- **No assumption that published == host-bound.** That assumption is exactly
  what breaks on colima; the design never relies on it.

## API / data model changes

No schema, ledger-format, or CLI-surface changes. `flywheel.yaml` and
`allocations.json` are untouched. Internal Go API only:

```go
// NEW — internal/cli/dockerports/dockerports.go
func PublishedPorts(ctx context.Context) (map[int]struct{}, error)
func parsePublishedPorts(dockerPsPorts []string) map[int]struct{} // unexported, table-tested

// CHANGED — internal/cli/allocator/allocator.go
// Inject the probe instead of the package-global var, so callers compose the
// docker-aware probe. The package var `portIsBindable` is removed; tests pass a
// stub probe directly.
func (f *File) Allocate(clientName, repoPath string, bindable func(int) bool) (Triple, error)
// PickFreePort(pool, taken, bindable) is unchanged.

// CHANGED — internal/cli/up/portheal.go
// planPortHeal already takes the probe; healHostPorts builds the docker-aware
// one (was: netutil.PortIsBindableWildcard).

// NEW — internal/cli/up (retry-on-collision wrapper around k3d create)
// Per-slot, reheal within the pool, retry once.
func createWithPortRetry(...) error
func isPortAllocatedErr(err error) bool

// CHANGED — internal/cli/doctor/full.go
// The port check uses the composed docker-aware probe, guarded by
// RegistryExists/ClusterRunning so this cluster's own ports aren't flagged.
```

Callers updated: `initcmd` (passes the composed probe into `Allocate`), `up`
(portheal probe + the two create call sites at `up.go:162`/cluster create),
`doctor` (composed probe + owned-guard).

## Migration plan

Greenfield behaviorally — no data or format migration:

- **No data migration.** `allocations.json` schema and pool ranges are unchanged;
  existing allocations keep working.
- **Backwards compatibility.** Pure behavior fix. The only externally visible
  change is that `init`/`up` now decline ports docker already holds (previously
  they'd hand them out and crash later). A port that was *correctly* free before
  is still chosen.
- **Fallback path.** If `docker ps` fails, the composed probe degrades to the
  current host-only behavior with a warning — never worse than today.
- **Rollout.** Single PR on `fix/colima-port-resolution`. No feature flag; the
  fix is strictly safer than the status quo. Gated by the existing Linux
  `k3d-e2e` job plus manual colima acceptance.

## Test plan

- **Unit — parser** (`dockerports`): table-driven over real `docker ps` `{{.Ports}}`
  strings — `0.0.0.0:`, `[::]:`, `127.0.0.1:`, host-IP-specific, port ranges,
  udp (ignored), container-only `5000/tcp` (ignored), multiple mappings per line,
  empty.
- **Unit — composed probe / allocator**: with a stubbed docker set, prove
  `Allocate` and `planPortHeal` **skip a docker-published port even when the host
  probe returns bindable=true** — the exact colima case. Also prove fallback
  (docker query errors → host-only behavior) and that an own-cluster `owned`
  port is still left untouched (portheal idempotency, `portheal.go`).
- **Unit — retry net**: `isPortAllocatedErr` matches the docker daemon strings
  and rejects unrelated errors; `createWithPortRetry` retries exactly once after
  a successful reheal and propagates other errors unchanged.
- **Unit — doctor**: the docker-aware port check flags a foreign docker-held port
  but does NOT flag a port held by *this* cluster's own registry/serverlb
  (owned-guard via `RegistryExists`/`ClusterRunning`).
- **Integration — keep CI green**: the Linux `k3d-e2e` port-heal assertion in
  `scripts/e2e.sh` (squat the http_port, expect `up` to heal) must still pass.
- **Manual acceptance (colima)**: reproduce the original failure — bring up one
  cluster, then `init`+`up` a second whose pool would collide; expect `up` to
  **heal and succeed** instead of crashing at registry/cluster create. This is
  the scenario in `docs/dev/dev-loop-validation.md` (the "Known gotchas" colima
  entry can then be downgraded to "handled").
- **Edge cases**: many containers (`docker ps` volume); a port held by a
  non-docker host process (host term still catches it); docker daemon down
  (graceful fallback + warning).

## Decisions

The five design questions are resolved (2026-06-30):

1. **Doctor consistency — INCLUDED.** `doctor`'s port check adopts the docker-aware
   probe with the `RegistryExists`/`ClusterRunning` owned-guard, so it proactively
   warns about a collision before `up` runs without false-alarming on this
   cluster's own ports.
2. **Helper home — NEW PACKAGE.** `internal/cli/dockerports` (a docker-generic
   query doesn't belong under the k3d-named package).
3. **Port sources — `docker ps` ALONE.** It already includes k3d's own containers
   and any foreign squatter; stopped containers don't hold ports, and flywheel's
   own clusters stay reserved via `allocations.json`. No `k3d list` cross-check.
4. **Retry net — ONCE, PER-SLOT, WITHIN-POOL.** On a docker port-allocation error,
   reheal the affected slot from its pool and retry the create exactly once;
   otherwise surface the error.
5. **Docker access — SHELL `docker ps`.** Matches the codebase's exec pattern, no
   new dependency; the `{{.Ports}}` parser is table-tested.

## Open questions

None remaining — the five above are resolved.
