// Package sourcemode owns the local-only ⇄ remote-backed source lifecycle that
// used to be reimplemented five times: the guard in add-app, the flip in
// publish-app, the consume step in up, the report in doctor, and the
// app↔worktree↔local-only join inlined in publish-app's shell completion. It
// answers one question in one place — which declared apps build from a
// local-only worktree, and is that allowed on the current branch?
//
// Layering. The per-entry invariant ("exactly one of url/local_only") and the
// raw local-only predicate live on schema.WorkspaceRepo, because schema.Validate
// is a first-class consumer and Go forbids the schema→sourcemode back-edge; this
// package re-exports them (IsLocalOnly, ValidSource) so callers reach the whole
// lifecycle through one import. Everything ABOVE a single entry — the join
// across declared apps × the workspace block, and the integration-branch guard —
// lives here.
//
// The join key is the worktree: a per-app GitRepository's spec.url always points
// at the in-cluster bare repo (`.../<worktree>.git`), so basename(spec.url)−".git"
// is the worktree the app builds from (see worktree.ParseAppGitRepository). The
// workspace block in flywheel.yaml records each worktree's true source, keyed by
// that same worktree name.
//
// scripts/ci/check-local-only.sh is a frozen bash re-implementation of this
// join+guard shipped into client repos (it can't be updated in-place once
// shipped). The agreement test in internal/cli/clientci proves the bash verdict
// and this package's Guard verdict stay in lockstep on a shared fixture corpus.
package sourcemode

import (
	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/worktree"
)

// IsLocalOnly reports whether a resolved workspace entry is local-only — the
// publishable state publish-app checks and the join keys on. It reads the raw
// local_only flag (mirroring check-local-only.sh's `select(.local_only == true)`
// and schema.File.LocalOnlyWorktrees), not the exactly-one invariant, so it
// agrees with the shell guard even on a malformed entry an unvalidated load
// might carry.
func IsLocalOnly(r schema.WorkspaceRepo) bool { return r.LocalOnly }

// ValidSource re-exports schema.WorkspaceRepo.ValidSource — the "exactly one of
// url/local_only" invariant — so this package surfaces the whole lifecycle. Its
// single definition lives on the schema type (schema.Validate consumes it too).
func ValidSource(r schema.WorkspaceRepo) bool { return r.ValidSource() }

// App is one declared app (builders/base/<Name>/gitrepository.yaml) joined to
// its workspace-block source entry, keyed by Worktree. Repo is the zero value
// when Declared is false — an app referencing a worktree the workspace block
// does not declare (a pre-workspace / legacy app), which every caller treats as
// not-local-only.
type App struct {
	Name     string               // app metadata.name
	Worktree string               // worktree it builds from (basename of spec.url minus ".git")
	Repo     schema.WorkspaceRepo // the workspace entry declaring Worktree
	Declared bool                 // a workspace entry exists for Worktree
}

// LocalOnly reports whether the app builds from a local-only worktree.
func (a App) LocalOnly() bool { return a.Declared && IsLocalOnly(a.Repo) }

// Join reads every declared app under repoDir (builders/base/*/gitrepository.yaml)
// and pairs it with its workspace-block entry from cfg, keyed by worktree. Apps
// whose manifest can't be parsed are skipped (best-effort, matching
// worktree.DeclaredApps); order follows worktree.DeclaredApps (glob order).
func Join(repoDir string, cfg *schema.File) ([]App, error) {
	declared, err := worktree.DeclaredApps(repoDir)
	if err != nil {
		return nil, err
	}
	apps := make([]App, 0, len(declared))
	for _, d := range declared {
		repo, ok := cfg.WorkspaceRepo(d.Worktree)
		apps = append(apps, App{
			Name:     d.Name,
			Worktree: d.Worktree,
			Repo:     repo,
			Declared: ok,
		})
	}
	return apps, nil
}

// LocalOnlyApps returns the declared apps that build from a local-only worktree
// — the set publish-app completion offers and the CI/pre-commit guard blocks.
func LocalOnlyApps(repoDir string, cfg *schema.File) ([]App, error) {
	all, err := Join(repoDir, cfg)
	if err != nil {
		return nil, err
	}
	var out []App
	for _, a := range all {
		if a.LocalOnly() {
			out = append(out, a)
		}
	}
	return out, nil
}

// Undeclared returns the declared apps whose worktree has no workspace-block
// entry — apps referencing a worktree nothing declares. doctor and up warn about
// these (up only when the worktree is also missing on disk). Apps with an empty
// worktree (an unparseable spec.url) are skipped, matching the prior inline
// `a.Worktree != ""` guard.
func Undeclared(repoDir string, cfg *schema.File) ([]App, error) {
	all, err := Join(repoDir, cfg)
	if err != nil {
		return nil, err
	}
	var out []App
	for _, a := range all {
		if !a.Declared && a.Worktree != "" {
			out = append(out, a)
		}
	}
	return out, nil
}

// Verdict is the guard's decision for the repo's current state on a target
// branch. It maps 1:1 onto check-local-only.sh's exit-code contract: Block ↔
// exit 1, Warn/Allow ↔ exit 0.
type Verdict int

const (
	// Allow: no app builds from a local-only worktree; nothing to guard.
	Allow Verdict = iota
	// Warn: local-only apps are present but the target branch is not the
	// integration branch — proceed, but remind the developer to publish.
	Warn
	// Block: local-only apps are present AND the target is the integration
	// branch — refuse, because their source exists only on one machine.
	Block
)

// OnIntegrationBranch reports whether target is the integration branch — the
// condition under which local-only apps are refused. Both the add-app guard and
// check-local-only.sh key their block on exactly this equality.
func OnIntegrationBranch(target, integration string) bool {
	return target == integration
}

// Guard returns the verdict for committing a repo carrying localOnly apps on the
// target branch. It is the Go expression of check-local-only.sh's rule; the
// agreement test asserts (Guard == Block) matches the script's non-zero exit on
// the same fixtures.
func Guard(localOnly []App, target, integration string) Verdict {
	if len(localOnly) == 0 {
		return Allow
	}
	if OnIntegrationBranch(target, integration) {
		return Block
	}
	return Warn
}
