// Package converge holds the cluster-convergence primitives used by
// `flywheel up`: reading + merging flywheel.yaml, rendering the bootstrap
// flux-system tree, and applying the dev-loop overlay / flywheel-config
// ConfigMap / waiting for Deployments. These are pure move-outs from
// package `up` — behaviour is unchanged.
package converge

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cobr-io/flywheel/internal/cli/config"
	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/naming"
)

func LoadConfig(repoDir string) (*schema.File, error) {
	commitedPath := filepath.Join(repoDir, naming.ConfigFile)
	committed, err := os.ReadFile(commitedPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", commitedPath, err)
	}
	// Validate the committed file on its own: it MUST NOT carry
	// paths.workspaces_root (that's .local-only). See design § flywheel.yaml.
	committedFile, err := schema.Parse(committed)
	if err != nil {
		return nil, err
	}
	if err := schema.Validate(committedFile); err != nil {
		return nil, fmt.Errorf("validate flywheel.yaml: %w", err)
	}

	var local []byte
	localPath := filepath.Join(repoDir, naming.ConfigFileLocal)
	if data, err := os.ReadFile(localPath); err == nil {
		local = data
		localFile, err := schema.Parse(local)
		if err != nil {
			return nil, err
		}
		if err := schema.ValidateLocal(localFile); err != nil {
			return nil, fmt.Errorf("validate flywheel.yaml.local: %w", err)
		}
	}

	// Merge for runtime use. The merged result legitimately carries
	// paths.workspaces_root from .local, so we do NOT re-run the
	// committed-file Validate against it.
	merged, err := config.MergeYAML(committed, local)
	if err != nil {
		return nil, err
	}
	return schema.Parse(merged)
}
