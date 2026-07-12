// Package converge holds the cluster-convergence primitives used by
// `flywheel up`: reading + merging flywheel.yaml, rendering the bootstrap
// flux-system tree, and applying the dev-loop overlay / flywheel-config
// ConfigMap / waiting for Deployments. These are pure move-outs from
// package `up` — behaviour is unchanged.
package converge

import (
	"github.com/cobr-io/flywheel/internal/cli/config"
	"github.com/cobr-io/flywheel/internal/cli/schema"
)

// LoadConfig is `up`'s config load: the full-validation preset of the shared
// loader. It validates the committed flywheel.yaml and the flywheel.yaml.local
// overlay before merging, so `up` refuses to run on an invalid config.
func LoadConfig(repoDir string) (*schema.File, error) {
	return config.Load(repoDir, config.LoadOptions{Validate: config.ValidateFull})
}
