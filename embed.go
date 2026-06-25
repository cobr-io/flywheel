// Package flywheel exposes the on-disk asset tree (templates,
// manifests, per-app template) as an embed.FS, so the compiled binary
// is fully self-contained and `flywheel init` / `up` / `add-app` don't
// need a git clone of github.com/cobr-io/flywheel at runtime.
//
// The on-disk directories (templates/, manifests/) are kept *as files*
// in the repo for dev iteration — you can `kubectl kustomize
// manifests/dev-loop/base/` directly when hacking. The binary's view of
// those directories is the embed.FS snapshotted at build time.
package flywheel

import "embed"

// Assets is the binary's full embedded asset tree:
//   - templates/client-skeleton/ — what `flywheel init` writes out.
//   - manifests/dev-loop/        — bootstrap-applied + Flux-reconciled.
//   - manifests/infra/           — local TLS infra (traefik wiring).
//   - manifests/per-app-template/ — consumed by `flywheel add app`.
//   - docs/guides/               — client-facing guides `flywheel init`
//     copies into <repo>/docs/ (single source:
//     the file lives here, not duplicated).
//
//go:embed all:templates all:manifests docs/guides
var Assets embed.FS

// BuildVersion labels which Flywheel release a client was scaffolded
// against. Overridable at link time via
//
//	-ldflags "-X github.com/cobr-io/flywheel.BuildVersion=vX.Y.Z"
//
// In a dev build (default), it's "v0.0.0-dev". The embedded assets are
// the source of truth at runtime; this constant exists only as a
// human-readable label.
var BuildVersion = "v0.0.0-dev"
