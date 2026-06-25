// Package down implements `flywheel down` per design § CLI surface.
// `down` tears the environment down: it deletes the k3d cluster + its
// registry and releases the allocator entry. It is destructive and
// requires confirmation (or --yes).
//
// Flywheel deliberately has no "stop, preserve state" command: k3d
// reassigns container IPs across stop/start, leaving stale Node IPs that
// break apiserver→kubelet (port-forward/exec/logs) on the multi-node
// topology. `up` is cheap enough to recreate from scratch, so the only
// teardown verb is a clean delete.
package down

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/cobr-io/flywheel/internal/cli/allocator"
	"github.com/cobr-io/flywheel/internal/cli/config"
	"github.com/cobr-io/flywheel/internal/cli/k3d"
	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/style"
)

// Options captures the user-facing knobs for `down`.
type Options struct {
	RepoDir         string
	Yes             bool   // bypass confirmation prompt
	HomeOverride    string // tests inject a custom HOME
	AllocationsPath string // tests inject a custom allocations.json path
	Stdin           io.Reader
}

// Down deletes the cluster + registry + allocator entry. Requires
// interactive confirmation unless `Yes` is set.
func Down(ctx context.Context, opts Options, out io.Writer) error {
	cfg, err := loadConfig(opts.RepoDir)
	if err != nil {
		return err
	}
	if !opts.Yes {
		if err := confirmDestructive(opts.Stdin, out, cfg.Cluster.Name); err != nil {
			return err
		}
	}
	if err := style.Spin(out, fmt.Sprintf("deleting cluster %s", cfg.Cluster.Name), func() error {
		return k3d.DeleteCluster(ctx, cfg.Cluster.Name)
	}); err != nil {
		return fmt.Errorf("delete cluster: %w", err)
	}
	if err := style.Spin(out, fmt.Sprintf("deleting registry %s", cfg.Cluster.Registry), func() error {
		return k3d.DeleteRegistry(ctx, cfg.Cluster.Registry)
	}); err != nil {
		return fmt.Errorf("delete registry: %w", err)
	}
	// Release allocator entry.
	allocPath := opts.AllocationsPath
	if allocPath == "" {
		if opts.HomeOverride != "" {
			allocPath = filepath.Join(opts.HomeOverride, ".config", "flywheel", "allocations.json")
		} else {
			p, err := allocator.DefaultPath()
			if err != nil {
				return err
			}
			allocPath = p
		}
	}
	if a, err := allocator.Load(allocPath); err == nil {
		a.Release(cfg.Client.Name)
		_ = a.Save(allocPath)
		style.OK(out, "released allocator entry for %s", cfg.Client.Name)
	}
	style.Summary(out, "done.")
	return nil
}

func confirmDestructive(stdin io.Reader, out io.Writer, clusterName string) error {
	if stdin == nil {
		stdin = os.Stdin
	}
	fmt.Fprintf(out, "About to delete k3d cluster %q and its registry.\n", clusterName)
	fmt.Fprintf(out, "PVCs are k3d-managed and will be removed with the cluster.\n")
	fmt.Fprintf(out, "Type %q to confirm: ", clusterName)
	r := bufio.NewReader(stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(line) != clusterName {
		return fmt.Errorf("confirmation didn't match; aborted")
	}
	return nil
}

func loadConfig(repoDir string) (*schema.File, error) {
	committed, err := os.ReadFile(filepath.Join(repoDir, "flywheel.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read flywheel.yaml: %w", err)
	}
	var local []byte
	if data, err := os.ReadFile(filepath.Join(repoDir, "flywheel.yaml.local")); err == nil {
		local = data
	}
	merged, err := config.MergeYAML(committed, local)
	if err != nil {
		return nil, err
	}
	f, err := schema.Parse(merged)
	if err != nil {
		return nil, err
	}
	// Don't full-validate here — destroy should still work even if the
	// committed file is mid-edit, as long as cluster.name + client.name
	// are present.
	if f.Cluster.Name == "" || f.Client.Name == "" {
		return nil, fmt.Errorf("flywheel.yaml missing cluster.name or client.name")
	}
	return f, nil
}
