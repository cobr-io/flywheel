#!/usr/bin/env bash
#
# Flywheel installer — downloads a prebuilt release binary and puts it on PATH.
#
#   curl -sSL https://raw.githubusercontent.com/cobr-io/flywheel/main/install.sh | bash
#
# It resolves a version *at run time* (latest by default) and installs it; it
# does not track updates or auto-upgrade. Re-run it to upgrade — the install is
# idempotent and version-aware (it no-ops when the target is already installed).
# This matches Flywheel's versioning model, where the pin in flywheel.yaml — not
# a floating :latest — is the source of truth.
#
# Environment overrides:
#   TAG=vX.Y.Z       install a specific release      (default: latest)
#   INSTALL_DIR=DIR  install location                (default: /usr/local/bin)
#   USE_SUDO=false   never use sudo                  (default: auto — sudo only
#                                                     if INSTALL_DIR isn't writable)
#   FORCE=true       reinstall even when the target version is already present
#
set -euo pipefail

REPO="cobr-io/flywheel"
BINARY="flywheel"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
USE_SUDO="${USE_SUDO:-auto}"
FORCE="${FORCE:-false}"
TAG="${TAG:-}"

# ---- logging (color only on a TTY) ----------------------------------------
if [ -t 2 ]; then
  BOLD=$(printf '\033[1m'); RED=$(printf '\033[31m'); GREEN=$(printf '\033[32m')
  YELLOW=$(printf '\033[33m'); DIM=$(printf '\033[2m'); RESET=$(printf '\033[0m')
else
  BOLD=""; RED=""; GREEN=""; YELLOW=""; DIM=""; RESET=""
fi
info() { printf '%s %s\n' "${BOLD}flywheel${RESET}" "$*" >&2; }
warn() { printf '%swarning:%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }
die()  { printf '%serror:%s %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }

# ---- prerequisites --------------------------------------------------------
need curl
need tar
need uname

SHACMD=""
if command -v sha256sum >/dev/null 2>&1; then
  SHACMD="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  SHACMD="shasum -a 256"
fi

# ---- detect platform ------------------------------------------------------
case "$(uname -s)" in
  Linux)  OS="linux" ;;
  Darwin) OS="darwin" ;;
  *) die "unsupported OS: $(uname -s) — Flywheel ships darwin and linux; on Windows use WSL2" ;;
esac

case "$(uname -m)" in
  x86_64|amd64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) die "unsupported architecture: $(uname -m) — Flywheel ships amd64 and arm64" ;;
esac

# ---- resolve the target version -------------------------------------------
# Follow the /releases/latest redirect: no API token, no rate limits. GitHub
# 302s to .../releases/tag/vX.Y.Z when a release exists, and 404s otherwise.
resolve_latest() {
  local effective
  effective=$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
    "https://github.com/${REPO}/releases/latest") || return 1
  case "$effective" in
    */releases/tag/*) printf '%s\n' "${effective##*/tag/}" ;;
    *) return 1 ;;
  esac
}

if [ -z "$TAG" ]; then
  info "resolving latest release…"
  TAG=$(resolve_latest) || die \
"could not resolve the latest release — the project may have no published
       releases yet. Pass TAG=vX.Y.Z, or build from source:
       https://github.com/${REPO}#installation"
fi
case "$TAG" in v*) ;; *) TAG="v${TAG}" ;; esac  # release dir keeps the leading v
VER="${TAG#v}"                                   # asset filename strips it

# ---- idempotency: is the target already installed? ------------------------
installed_version() {
  local bin=""
  if [ -x "${INSTALL_DIR}/${BINARY}" ]; then
    bin="${INSTALL_DIR}/${BINARY}"
  elif command -v "$BINARY" >/dev/null 2>&1; then
    bin="$BINARY"
  else
    return 1
  fi
  "$bin" version 2>/dev/null | awk '{print $2}'
}

CUR="$(installed_version || true)"
if [ -z "$CUR" ]; then
  info "installing ${TAG}"
elif [ "${CUR#v}" = "$VER" ]; then
  if [ "$FORCE" != "true" ]; then
    info "${GREEN}already on ${TAG}${RESET} — nothing to do (FORCE=true to reinstall)"
    exit 0
  fi
  info "reinstalling ${TAG}"
else
  info "upgrading ${CUR} → ${TAG}"
fi

# ---- download -------------------------------------------------------------
ASSET="${BINARY}_${VER}_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/${REPO}/releases/download/${TAG}"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

info "downloading ${ASSET}…"
curl -fsSL "${BASE}/${ASSET}" -o "${TMP}/${ASSET}" \
  || die "download failed: ${BASE}/${ASSET}"

# ---- verify checksum (best-effort) ----------------------------------------
if [ -n "$SHACMD" ]; then
  SUMS=""
  for name in "${BINARY}_${VER}_checksums.txt" "checksums.txt"; do
    if curl -fsSL "${BASE}/${name}" -o "${TMP}/checksums.txt" 2>/dev/null; then
      SUMS="${TMP}/checksums.txt"; break
    fi
  done
  if [ -n "$SUMS" ]; then
    want="$(awk -v f="$ASSET" '$2 == f {print $1}' "$SUMS")"
    if [ -n "$want" ]; then
      got="$($SHACMD "${TMP}/${ASSET}" | awk '{print $1}')"
      [ "$want" = "$got" ] || die "checksum mismatch for ${ASSET} (want ${want}, got ${got})"
      info "${DIM}checksum verified${RESET}"
    else
      warn "no checksum entry for ${ASSET}; skipping verification"
    fi
  else
    warn "no checksums file published for ${TAG}; skipping verification"
  fi
else
  warn "no sha256 tool (sha256sum/shasum) found; skipping checksum verification"
fi

# ---- extract --------------------------------------------------------------
tar -xzf "${TMP}/${ASSET}" -C "$TMP"
[ -f "${TMP}/${BINARY}" ] || die "archive did not contain a '${BINARY}' binary"
chmod +x "${TMP}/${BINARY}"

# ---- install (sudo only when needed) --------------------------------------
run() {
  if [ "$USE_SUDO" = "false" ]; then
    "$@"
  elif [ -w "$INSTALL_DIR" ] || { [ ! -e "$INSTALL_DIR" ] && [ -w "$(dirname "$INSTALL_DIR")" ]; }; then
    "$@"
  else
    command -v sudo >/dev/null 2>&1 || die \
      "cannot write ${INSTALL_DIR} and sudo is unavailable — re-run with INSTALL_DIR=\$HOME/.local/bin"
    info "${DIM}elevating with sudo to write ${INSTALL_DIR}${RESET}"
    sudo "$@"
  fi
}

run mkdir -p "$INSTALL_DIR"
run install -m 0755 "${TMP}/${BINARY}" "${INSTALL_DIR}/${BINARY}"

# ---- verify + next steps --------------------------------------------------
DEST="${INSTALL_DIR}/${BINARY}"
info "${GREEN}installed${RESET} $("$DEST" version 2>/dev/null || echo "${BINARY} ${TAG}") → ${DEST}"

case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *) warn "${INSTALL_DIR} is not on your PATH — add it:  export PATH=\"${INSTALL_DIR}:\$PATH\"" ;;
esac

printf '\n' >&2
printf '%sNext steps%s\n' "$BOLD" "$RESET" >&2
printf '  %sflywheel doctor%s   check host prerequisites (git, k3d, docker, mkcert)\n' "$BOLD" "$RESET" >&2
printf '  %sflywheel init%s     scaffold a GitOps repo in the current directory\n' "$BOLD" "$RESET" >&2
printf '  %sflywheel up%s       bring up the local cluster\n' "$BOLD" "$RESET" >&2
printf '\nDocs: https://github.com/%s#readme\n' "$REPO" >&2
