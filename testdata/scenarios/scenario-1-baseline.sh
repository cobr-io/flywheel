#!/usr/bin/env bash
# Scenario 1 (plan § 1.4 / T1.7): baseline commit on main.
# Edit code on the sibling app repo's main, commit; assert a pod runs the
# new image within 60s.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
source ./lib.sh

log "scenario 1: baseline commit on main"

create_sibling_app_repo
add_sample_app

# git-auto-sync mirrors the sibling repo into the in-cluster git-server;
# image-builder-controller builds it; IUA bumps the tag; the app rolls.
# First build is slowest: a 2-core CI runner waits for the dependsOn chain
# (flywheel-dev-loop → client-builders) plus a cold kaniko build + scan +
# rollout. Allow 480s. The steady-state assertion (150s) is below.
t0=$SECONDS
edit_app_and_commit main "hello from sample-app v1"
wait_for_served_text "hello from sample-app v1" "$(scaled 480)"
v1_dur=$((SECONDS - t0))

# Second and third commits: two warm-leg samples, BOTH latency-gated. The
# historical per-leg bimodality (issue #107) had two causes, both fixed: the
# per-node cold pull of the buildkit client image (up's mirror-buildkit-client
# step) and this harness probing the old Terminating pod (wait_for_served_text
# now reads the newest pod). Post-fix CI reads 7s per warm leg.
t0=$SECONDS
edit_app_and_commit main "hello from sample-app v2"
wait_for_served_text "hello from sample-app v2" "$(scaled 150)"
v2_dur=$((SECONDS - t0))

t0=$SECONDS
edit_app_and_commit main "hello from sample-app v3"
wait_for_served_text "hello from sample-app v3" "$(scaled 150)"
v3_dur=$((SECONDS - t0))

cycle_report "cold_v1=${v1_dur}s warm_v2=${v2_dur}s warm_v3=${v3_dur}s"

# Mechanism guard first: all fast-path pokes fired, none disabled or
# mistargeted. Binary and contention-immune — when the loop is slow, this
# names the broken hop before the wall-clock gate below fires.
assert_dev_loop_pokes

# Latency gate on EVERY warm leg (was min-of-two while #107's per-leg coin
# flips were live). Healthy warm legs are ~7-13s on these same 2-core
# runners; a hop falling back to a Flux poll interval lands 20s+. The 30s
# ceiling is deliberately UNSCALED: runner speed is already priced into the
# healthy measurements, and scaling would swallow exactly the
# interval-fallback signal the gate exists to catch.
for leg in "v2:$v2_dur" "v3:$v3_dur"; do
  if (( ${leg#*:} > 30 )); then
    log "LATENCY GATE: warm leg ${leg%%:*} took ${leg#*:}s (healthy ≤13s, gate 30s) — a fast-path hop is falling back to its poll interval"
    dump_diag
    exit 1
  fi
done

# --- #147/#148 upgrade check ------------------------------------------------
# Reproduce a pre-#148 cluster: before the Recreate migration, git-auto-sync
# and git-deploy-controller carried no `strategy:` field at all, so the API
# server server-defaulted spec.strategy to {type: RollingUpdate,
# rollingUpdate: {maxSurge: 25%, maxUnavailable: 25%}}. A plain merge patch
# setting only `type: RollingUpdate` reproduces that exact shape — the API
# server's own defaulting fills rollingUpdate back in on write, the same way
# it did for every pre-#148 apply — without touching the pod template, so
# nothing restarts. `flywheel up` must then migrate both Deployments back to
# Recreate with no leftover rollingUpdate: before the applier's #147 fix, the
# plain SSA apply here fails outright ("spec.strategy.rollingUpdate:
# Forbidden: may not be specified when strategy `type` is 'Recreate'" — SSA
# can't drop a field no field manager owns), so `flywheel up` would exit
# non-zero and this scenario would fail under `set -e`.
log "#147 upgrade check: reverting dev-loop controllers to the pre-#148 RollingUpdate shape"
for dep in git-auto-sync git-deploy-controller; do
  kc -n flywheel-system patch deployment "$dep" --type=merge \
    -p '{"spec":{"strategy":{"type":"RollingUpdate"}}}'
  log "  $dep reverted: strategy.type=$(kc -n flywheel-system get deployment "$dep" -o jsonpath='{.spec.strategy.type}') rollingUpdate=$(kc -n flywheel-system get deployment "$dep" -o jsonpath='{.spec.strategy.rollingUpdate}')"
done

log "#147 upgrade check: re-running flywheel up to migrate the live Deployments"
( cd "$CLIENT_REPO" && flywheel up )

for dep in git-auto-sync git-deploy-controller; do
  strategy_type=$(kc -n flywheel-system get deployment "$dep" -o jsonpath='{.spec.strategy.type}')
  rolling_update=$(kc -n flywheel-system get deployment "$dep" -o jsonpath='{.spec.strategy.rollingUpdate}')
  if [[ "$strategy_type" != "Recreate" || -n "$rolling_update" ]]; then
    log "FAIL: Deployment $dep strategy.type=$strategy_type rollingUpdate=${rolling_update:-<none>} after up, want Recreate with no rollingUpdate"
    dump_diag
    exit 1
  fi
  log "Deployment $dep strategy = Recreate, no rollingUpdate (upgrade migration OK)"
done
log "#147 upgrade check PASS"

log "scenario 1 PASS"
