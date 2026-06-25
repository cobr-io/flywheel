#!/usr/bin/env bash
# SOPS-shape guard. Two rules:
#   1. Every *.enc.yaml / *.enc.yml file must have a SOPS envelope and
#      its data:/stringData: values must be ENC[AES256_GCM,...] blobs.
#   2. Any kind: Secret outside *.enc.* with non-empty data:/stringData:
#      is rejected as plaintext.
#
# Pre-commit invokes with explicit filenames; CI invokes with no args
# (the script discovers files itself via `git ls-files`).
#
# Depends on mikefarah's yq (https://github.com/mikefarah/yq).

set -euo pipefail

if [ "$#" -gt 0 ]; then
  files=("$@")
else
  mapfile -t files < <(git ls-files '*.yaml' '*.yml')
fi

exit_code=0
checked=0

for f in "${files[@]}"; do
  [ -f "$f" ] || continue
  checked=$((checked + 1))

  case "$f" in
    *.enc.yaml|*.enc.yml)
      # Rule 1a: must have a SOPS envelope (sops.mac key present).
      if ! yq -e '.sops.mac' "$f" >/dev/null 2>&1; then
        echo "ERROR: $f: missing sops envelope (no .sops.mac); encrypt with: sops --encrypt --in-place $f" >&2
        exit_code=1
        continue
      fi

      # Rule 1b: every value under data: / stringData: must be an ENC[...] blob.
      # Iterate values via yq; bail on first plaintext-shaped value.
      while IFS= read -r v; do
        [ -n "$v" ] || continue
        if [[ ! "$v" =~ ^ENC\[AES256_GCM, ]]; then
          echo "ERROR: $f: plaintext value found in data/stringData ('${v:0:40}...'); re-encrypt with: sops --encrypt --in-place $f" >&2
          exit_code=1
          break
        fi
      done < <(yq '(.data // {}) as $d | (.stringData // {}) as $s | ($d * $s) | to_entries[] | .value' "$f" 2>/dev/null || true)
      ;;
    *)
      # Rule 2: kind: Secret outside .enc.* must not have non-empty data: / stringData:.
      # yq on multi-doc files: iterate documents.
      doc_count=$(yq 'length' --output-format=json "$f" 2>/dev/null | head -1 || echo 0)
      doc_indices=$(yq 'documentIndex' "$f" 2>/dev/null | sort -u || echo "")
      for idx in $doc_indices; do
        kind=$(yq "select(documentIndex == $idx) | .kind" "$f" 2>/dev/null || true)
        [ "$kind" = "Secret" ] || continue
        data_keys=$(yq "select(documentIndex == $idx) | (.data // {}) + (.stringData // {}) | length" "$f" 2>/dev/null || echo 0)
        if [ "${data_keys:-0}" -gt 0 ]; then
          echo "ERROR: $f (doc $idx): kind: Secret with plaintext data/stringData; rename to *.enc.yaml and run: sops --encrypt --in-place" >&2
          exit_code=1
        fi
      done
      ;;
  esac
done

if [ "$exit_code" -eq 0 ]; then
  echo "SOPS-shape check: passed across $checked files."
fi

exit $exit_code
