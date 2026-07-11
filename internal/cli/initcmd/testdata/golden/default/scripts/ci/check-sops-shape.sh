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
# Any yq failure (e.g. malformed YAML) is treated as a check failure, not a
# silent pass — this is a security guard and must fail closed.
#
# Depends on mikefarah's yq (https://github.com/mikefarah/yq).
#
# Bash 3.2 compatible (no mapfile/readarray) — this runs as a pre-commit
# hook and in CI, and stock macOS ships /bin/bash 3.2.

set -euo pipefail

files=()
if [ "$#" -gt 0 ]; then
  files=("$@")
else
  while IFS= read -r f; do
    files+=("$f")
  done < <(git ls-files '*.yaml' '*.yml')
fi

exit_code=0
checked=0

for f in "${files[@]}"; do
  [ -f "$f" ] || continue
  checked=$((checked + 1))

  case "$f" in
    *.enc.yaml|*.enc.yml)
      # Rule 1a: must have a SOPS envelope (sops.mac key present). yq -e
      # already exits non-zero both when the key is absent/false *and* when
      # the file fails to parse, so this is fail-closed as written.
      if ! yq -e '.sops.mac' "$f" >/dev/null 2>&1; then
        echo "ERROR: $f: missing sops envelope (no .sops.mac); encrypt with: sops --encrypt --in-place $f" >&2
        exit_code=1
        continue
      fi

      # Rule 1b: every value under data: / stringData: must be an ENC[...] blob.
      # Iterate values via yq; bail on first plaintext-shaped value. A yq
      # failure (e.g. malformed YAML) is itself a check failure.
      # shellcheck disable=SC2016 # $d/$s are yq variables, not shell vars.
      if ! values="$(yq '(.data // {}) as $d | (.stringData // {}) as $s | ($d * $s) | to_entries[] | .value' "$f" 2>&1)"; then
        echo "ERROR: $f: failed to evaluate data/stringData ($values)" >&2
        exit_code=1
        continue
      fi
      while IFS= read -r v; do
        [ -n "$v" ] || continue
        if [[ ! "$v" =~ ^ENC\[AES256_GCM, ]]; then
          echo "ERROR: $f: plaintext value found in data/stringData ('${v:0:40}...'); re-encrypt with: sops --encrypt --in-place $f" >&2
          exit_code=1
          break
        fi
      done <<< "$values"
      ;;
    *)
      # Rule 2: kind: Secret outside .enc.* must not have non-empty data: / stringData:.
      # yq on multi-doc files: iterate documents. Any yq failure here is
      # itself a check failure.
      if ! doc_indices="$(yq 'documentIndex' "$f" 2>&1)"; then
        echo "ERROR: $f: failed to parse YAML ($doc_indices)" >&2
        exit_code=1
        continue
      fi
      doc_indices="$(printf '%s\n' "$doc_indices" | sort -u)"
      for idx in $doc_indices; do
        if ! kind="$(yq "select(documentIndex == $idx) | .kind" "$f" 2>&1)"; then
          echo "ERROR: $f (doc $idx): failed to parse YAML ($kind)" >&2
          exit_code=1
          continue
        fi
        [ "$kind" = "Secret" ] || continue
        if ! data_keys="$(yq "select(documentIndex == $idx) | (.data // {}) + (.stringData // {}) | length" "$f" 2>&1)"; then
          echo "ERROR: $f (doc $idx): failed to evaluate data/stringData ($data_keys)" >&2
          exit_code=1
          continue
        fi
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
