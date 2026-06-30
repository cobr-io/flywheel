package up

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/cobr-io/flywheel/internal/cli/k3d"
	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/style"
)

// isPortAllocatedErr reports whether err is docker/k3d failing to bind a host
// port. It matches the docker DAEMON's messages — identical across client OSes
// (macOS, Linux, WSL2) since they come from the daemon, not the client — rather
// than any OS-specific text:
//   - "port is already allocated" — docker's publish-conflict message
//   - "address already in use"    — the underlying bind error
func isPortAllocatedErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "port is already allocated") ||
		strings.Contains(s, "address already in use")
}

// retryOnPortCollision runs create; if it fails with a port-allocation error it
// runs reheal (clean up the partial + reallocate the port) and retries create
// exactly once. Any non-port error (or a reheal error) is returned unchanged.
// Pure orchestration, separated from the k3d/spin specifics so it's unit-tested.
func retryOnPortCollision(create, reheal func() error) error {
	err := create()
	if err == nil || !isPortAllocatedErr(err) {
		return err
	}
	if rerr := reheal(); rerr != nil {
		return rerr
	}
	return create()
}

// createRegistryHealOnce creates the k3d registry. The docker-aware portheal
// (step 5b) should have already cleared collisions, but a port can still be
// taken between that probe and this bind (TOCTOU). On such a failure it removes
// the partial registry, reheals registry_port from its pool, and retries once;
// any other error is returned unchanged.
func createRegistryHealOnce(ctx context.Context, opts Options, cfg *schema.File, out io.Writer) error {
	create := func() error {
		return style.Spin(out,
			fmt.Sprintf("k3d registry %s:%d", cfg.Cluster.Registry, cfg.Cluster.RegistryPort),
			func() error { return k3d.CreateRegistry(ctx, cfg.Cluster.Registry, cfg.Cluster.RegistryPort) },
		)
	}
	reheal := func() error {
		style.Warn(out, "registry_port %d was taken at create time; reallocating and retrying", cfg.Cluster.RegistryPort)
		// A failed `k3d registry create` can leave a registered-but-unstarted
		// registry; remove it so the retry creates cleanly on the healed port.
		if derr := k3d.DeleteRegistry(ctx, cfg.Cluster.Registry); derr != nil {
			return fmt.Errorf("clean up partial registry: %w", derr)
		}
		return healHostPorts(ctx, opts, cfg, out)
	}
	return retryOnPortCollision(create, reheal)
}

// createClusterHealOnce creates the k3d cluster. On a host-port collision it
// deletes the partial cluster, reheals http/https from their pools, and retries
// once. build is re-evaluated each attempt so the retry picks up the healed
// ports (the registry it depends on is left untouched — its port is "owned" and
// so skipped by the reheal). Any other error is returned unchanged.
func createClusterHealOnce(ctx context.Context, opts Options, cfg *schema.File, build func() k3d.CreateClusterOpts, out io.Writer) error {
	create := func() error {
		return style.Spin(out,
			fmt.Sprintf("k3d cluster %s", cfg.Cluster.Name),
			func() error { return k3d.CreateCluster(ctx, build()) },
		)
	}
	reheal := func() error {
		style.Warn(out, "a cluster host port (http %d / https %d) was taken at create time; reallocating and retrying",
			cfg.Cluster.HttpPort, cfg.Cluster.HttpsPort)
		if derr := k3d.DeleteCluster(ctx, cfg.Cluster.Name); derr != nil {
			return fmt.Errorf("clean up partial cluster: %w", derr)
		}
		return healHostPorts(ctx, opts, cfg, out)
	}
	return retryOnPortCollision(create, reheal)
}
