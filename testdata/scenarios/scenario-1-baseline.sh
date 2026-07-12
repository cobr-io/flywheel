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

# Third commit: genuinely steady state (issue #107's backoff window is over)
# — THIS leg carries the latency gate. Healthy is 7-13s including the warm
# image rebuild (docs/dev/dev-loop-latency.md); a dead poke quantizes a hop
# to its Flux interval and lands at ~36s+. The 30s ceiling is deliberately
# UNSCALED: the healthy numbers were measured on the same 2-core CI runners
# this gate runs on, so runner speed is already priced in, and scaling would
# swallow exactly the interval-fallback signal the gate exists to catch.
t0=$SECONDS
edit_app_and_commit main "hello from sample-app v3"
wait_for_served_text "hello from sample-app v3" "$(scaled 150)"
v3_dur=$((SECONDS - t0))

cycle_report "cold_v1=${v1_dur}s warm_v2=${v2_dur}s warm_v3=${v3_dur}s"

if (( v3_dur > 30 )); then
  log "LATENCY GATE: steady-state leg took ${v3_dur}s (healthy ≤13s, gate 30s) — a fast-path hop is falling back to its poll interval"
  dump_diag
  exit 1
fi

# Mechanism guard: all fast-path pokes fired, none disabled or mistargeted.
assert_dev_loop_pokes

log "scenario 1 PASS"
