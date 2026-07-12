package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/naming"
)

// ValidateLevel selects how strictly Load checks the on-disk documents before
// returning the merged config.
type ValidateLevel int

const (
	// ValidateNone parses + merges but runs no semantic validation. Commands
	// that must operate on a mid-edit or partial config (add app, publish-app,
	// use, doctor, down, clean) use this and assert only the specific fields
	// they need (via LoadOptions.RequireCluster or their own follow-up check).
	ValidateNone ValidateLevel = iota
	// ValidateLocalOnly validates the flywheel.yaml.local overlay
	// (schema.ValidateLocal) but not the committed file.
	ValidateLocalOnly
	// ValidateFull validates the committed flywheel.yaml (schema.Validate,
	// which also rejects .local-only fields such as paths.workspaces_root) AND
	// the flywheel.yaml.local overlay (schema.ValidateLocal). `flywheel up`
	// uses this — it is the one path that refuses to run on an invalid config.
	ValidateFull
)

// LoadOptions tunes Load per command. The zero value (ValidateNone,
// RequireCluster=false) is the most permissive load: read + merge only.
type LoadOptions struct {
	// Validate selects the strictness level (see ValidateLevel).
	Validate ValidateLevel
	// RequireCluster makes Load error when cluster.name is empty — for the
	// commands that resolve the k3d context from it (use, clean).
	RequireCluster bool
}

// Load is the single reader of flywheel.yaml(+.local) for every command. It
// reads the committed flywheel.yaml, optionally validates it, merges the
// flywheel.yaml.local overlay (deep-merge; arrays replaced wholesale — see
// MergeYAML), re-parses the merged document, and fills in load-time defaults.
//
// Validation, when requested, runs on the committed and .local documents
// SEPARATELY and BEFORE the merge: the committed file must not carry
// .local-only fields (schema.Validate enforces that), but the merged result
// legitimately does, so Validate is never re-run against the merge.
func Load(repoDir string, opts LoadOptions) (*schema.File, error) {
	committed, err := os.ReadFile(filepath.Join(repoDir, naming.ConfigFile))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", naming.ConfigFile, err)
	}
	if opts.Validate == ValidateFull {
		cf, err := schema.Parse(committed)
		if err != nil {
			return nil, err
		}
		if err := schema.Validate(cf); err != nil {
			return nil, fmt.Errorf("validate %s: %w", naming.ConfigFile, err)
		}
	}

	var local []byte
	if data, err := os.ReadFile(filepath.Join(repoDir, naming.ConfigFileLocal)); err == nil {
		local = data
		if opts.Validate == ValidateFull || opts.Validate == ValidateLocalOnly {
			lf, err := schema.Parse(local)
			if err != nil {
				return nil, err
			}
			if err := schema.ValidateLocal(lf); err != nil {
				return nil, fmt.Errorf("validate %s: %w", naming.ConfigFileLocal, err)
			}
		}
	}

	merged, err := MergeYAML(committed, local)
	if err != nil {
		return nil, err
	}
	cfg, err := schema.Parse(merged)
	if err != nil {
		return nil, err
	}
	applyLoadDefaults(cfg)

	if opts.RequireCluster && cfg.Cluster.Name == "" {
		return nil, fmt.Errorf("%s: cluster.name is required", naming.ConfigFile)
	}
	return cfg, nil
}

// Load-time defaults, filled by Load when the corresponding field is unset.
//
// These live here (loader-applied) rather than as schema accessor methods (the
// IntegrationBranch() pattern) on purpose: commands read the struct fields
// directly (cfg.Namespaces.Apps, cfg.Flux.IntervalLocal), so applying the
// default once here keeps it in ONE place without having to convert every
// reader to an accessor. For a fully-validated config they are no-ops:
// schema.Validate requires namespaces.apps and flux.interval_local to be
// non-empty, so only the not-fully-validated commands ever exercise them.
//
// cfg.Namespaces.Flywheel is a special case: nothing reads it any more (the
// namespace is fixed at naming.FlywheelNamespace, task T14). The default below
// is kept only so the struct field is populated for any generic serialization
// of an under-validated config; it feeds no behavior.
const (
	defaultAppsNamespace     = "apps"
	defaultFluxIntervalLocal = "10s"
)

func applyLoadDefaults(cfg *schema.File) {
	if cfg.Namespaces.Apps == "" {
		cfg.Namespaces.Apps = defaultAppsNamespace
	}
	if cfg.Namespaces.Flywheel == "" {
		cfg.Namespaces.Flywheel = naming.FlywheelNamespace
	}
	if cfg.Flux.IntervalLocal == "" {
		cfg.Flux.IntervalLocal = defaultFluxIntervalLocal
	}
}
