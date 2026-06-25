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
edit_app_and_commit main "hello from sample-app v1"
wait_for_served_text "hello from sample-app v1" 480

# Steady-state: a second commit rolls through a warm-cache build. The
# image build + Flux scan + IUA + rollout still spans several reconcile
# cycles, so allow 150s (the design's sub-30s target is the commit→build
# trigger latency, not the full cold+scan+roll wall time on CI/colima).
edit_app_and_commit main "hello from sample-app v2"
wait_for_served_text "hello from sample-app v2" 150

log "scenario 1 PASS"
