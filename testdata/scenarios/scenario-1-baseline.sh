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

# Second commit: still not steady state — the first one or two bumps after
# add-app race a 30s ImagePolicy backoff scheduled during the pre-first-push
# NAME_UNKNOWN window (issue #107), so this leg is bimodal (~7-13s or ~40s)
# and gets a liveness ceiling only, no latency gate.
t0=$SECONDS
edit_app_and_commit main "hello from sample-app v2"
wait_for_served_text "hello from sample-app v2" "$(scaled 150)"
v2_dur=$((SECONDS - t0))

# Third commit: a second warm-leg sample. The #107 race is per-leg (observed
# striking the THIRD bump in CI while the second was fast), so no single leg
# is gate-safe — the gate below uses the min of the two warm legs.
t0=$SECONDS
edit_app_and_commit main "hello from sample-app v3"
wait_for_served_text "hello from sample-app v3" "$(scaled 150)"
v3_dur=$((SECONDS - t0))

cycle_report "cold_v1=${v1_dur}s warm_v2=${v2_dur}s warm_v3=${v3_dur}s"

# Mechanism guard first: all fast-path pokes fired, none disabled or
# mistargeted. Binary and contention-immune — when the loop is slow, this
# names the broken hop before the wall-clock gate below fires.
assert_dev_loop_pokes

# Latency gate on min(warm legs): the #107 per-leg race can quantize any ONE
# leg to a Flux interval (+10..30s), so a single-leg gate false-positives on
# healthy code (~50% of runs). A SYSTEMIC regression — a dead poke, a broken
# controller — slows EVERY warm leg, so the min crossing 30s is the signal.
# Healthy warm legs are 7-13s on these same 2-core runners; the ceiling is
# deliberately UNSCALED because runner speed is already priced into those
# measurements, and scaling would swallow the interval-fallback signal.
warm_min=$(( v2_dur < v3_dur ? v2_dur : v3_dur ))
if (( warm_min > 30 )); then
  log "LATENCY GATE: both warm legs slow (v2=${v2_dur}s, v3=${v3_dur}s; healthy ≤13s, gate min>30s) — a fast-path hop is systematically falling back to its poll interval"
  dump_diag
  exit 1
fi

log "scenario 1 PASS"
