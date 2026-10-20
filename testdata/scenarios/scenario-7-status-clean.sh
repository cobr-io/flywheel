#!/usr/bin/env bash
# Scenario 7 (issue #38): `flywheel status` and `flywheel clean` against a live
# dev loop.
#   1. `status` exits 0 and reports a healthy cluster after up + scenario 1.
#   2. `clean` deletes a planted orphan (a managed-by=flywheel PVC the current
#      manifests don't declare) and KEEPS the live buildkit-cache and
#      git-server-data PVCs (issue #172: it used to delete them).
#   3. The dev loop still builds and serves a commit afterwards — the proof
#      that git-server and buildkitd kept their storage.
#
# Needs scenario 1's app; runs before the destructive scenario 5 (see
# run-all.sh).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
source ./lib.sh

log "scenario 7: status + clean"

# --- 1. status ------------------------------------------------------------------
status_out=$(mktemp)
trap 'rm -f "$status_out"' EXIT
if ! ( cd "$CLIENT_REPO" && flywheel status ) >"$status_out" 2>&1; then
  log "FAIL: flywheel status exited non-zero on a healthy cluster"
  cat "$status_out"; dump_diag; exit 1
fi
for want in "Flywheel status — $KCTX" "nodes: " "resources: "; do
  grep -qF -- "$want" "$status_out" ||
    { log "FAIL: flywheel status output missing '$want'"; cat "$status_out"; exit 1; }
done
log "flywheel status: exit 0, reports nodes and Flux resources"

# --- 2. clean -------------------------------------------------------------------
live_pvcs=(buildkit-cache git-server-data)
for p in "${live_pvcs[@]}"; do
  kc -n flywheel-system get pvc "$p" >/dev/null ||
    { log "FAIL: expected live PVC flywheel-system/$p before clean"; exit 1; }
done

orphan="orphan-e2e"
kc apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: $orphan
  namespace: flywheel-system
  labels:
    app.kubernetes.io/managed-by: flywheel
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Mi
EOF
log "planted orphan PVC flywheel-system/$orphan"

clean_out=$(cd "$CLIENT_REPO" && flywheel clean 2>&1) ||
  { log "FAIL: flywheel clean exited non-zero"; echo "$clean_out"; exit 1; }
echo "$clean_out"
grep -qF "cleaned 1 PVC(s) in flywheel-system" <<<"$clean_out" ||
  { log "FAIL: clean should delete exactly the one orphan"; exit 1; }

deadline=$((SECONDS + $(scaled 30)))
while kc -n flywheel-system get pvc "$orphan" >/dev/null 2>&1; do
  (( SECONDS < deadline )) || { log "FAIL: orphan PVC $orphan still present after clean"; exit 1; }
  sleep 2
done
log "clean removed the orphan PVC"

for p in "${live_pvcs[@]}"; do
  deleting=$(kc -n flywheel-system get pvc "$p" -o jsonpath='{.metadata.deletionTimestamp}' 2>/dev/null) ||
    { log "FAIL: clean deleted live PVC flywheel-system/$p"; exit 1; }
  [[ -z "$deleting" ]] ||
    { log "FAIL: clean marked live PVC flywheel-system/$p for deletion (deletionTimestamp=$deleting)"; exit 1; }
done
log "clean kept the live PVCs: ${live_pvcs[*]}"

# --- 3. the dev loop still works ------------------------------------------------
switch_app_branch main 2>/dev/null || true
switch_gitops_branch main 2>/dev/null || true
edit_app_and_commit main "hello after clean"
wait_for_served_text "hello after clean" "$(scaled 150)"

log "scenario 7 PASS"
