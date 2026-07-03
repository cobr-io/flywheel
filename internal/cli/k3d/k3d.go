// Package k3d wraps the k3d CLI for cluster + registry lifecycle.
// `flywheel up` steps 6-7 and `flywheel down` shell out to this package
// (per design § Prerequisites: k3d remains an OS dep, not embedded). All
// commands stream their output to the caller's stdout so users see
// progress on long-running operations.
package k3d

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strings"
)

// WorkspaceVisible reports whether relPath exists under /workspaces inside the
// cluster's server node — i.e. whether the host workspaces_root bind-mount
// actually bridged into k3d. A nil error with ok=false means the path is
// definitively absent (the mount didn't bridge); a non-nil error means the
// probe itself failed (treat as inconclusive, don't block on it).
func WorkspaceVisible(ctx context.Context, clusterName, relPath string) (bool, error) {
	node := fmt.Sprintf("k3d-%s-server-0", clusterName)
	cmd := exec.CommandContext(ctx, "docker", "exec", node, "test", "-e", path.Join("/workspaces", relPath))
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, nil // `test -e` exits 1 when the path is absent
	}
	return false, err // docker exec itself failed (node name, daemon, …)
}

// NodePathExists reports whether absPath exists inside the cluster's server
// node — used to verify a host-absolute bind-mount (e.g. a git worktree's shared
// git dir, issue #62) actually bridged into k3d. Same probe semantics as
// WorkspaceVisible: nil error with ok=false means definitively absent; a non-nil
// error means the probe itself failed (treat as inconclusive).
func NodePathExists(ctx context.Context, clusterName, absPath string) (bool, error) {
	node := fmt.Sprintf("k3d-%s-server-0", clusterName)
	cmd := exec.CommandContext(ctx, "docker", "exec", node, "test", "-e", absPath)
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return false, nil
	}
	return false, err
}

// CreateRegistry creates a k3d-managed image registry on the host. The
// resulting in-cluster DNS name is `k3d-<name>:<port>`. Idempotent:
// returns nil if a registry of this name already exists.
func CreateRegistry(ctx context.Context, name string, port int) error {
	if exists, err := registryExists(ctx, name); err != nil {
		return err
	} else if exists {
		return nil
	}
	cmd := exec.CommandContext(ctx, "k3d", "registry", "create", name,
		"--port", fmt.Sprintf("%d", port))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("k3d registry create %s: %w\n%s", name, err, out)
	}
	return nil
}

// DeleteRegistry removes the registry. Idempotent.
func DeleteRegistry(ctx context.Context, name string) error {
	// k3d prefixes with "k3d-" automatically when looking up; pass the
	// raw name. Deleting a non-existent registry returns non-zero, so
	// pre-check.
	if exists, err := registryExists(ctx, name); err != nil {
		return err
	} else if !exists {
		return nil
	}
	cmd := exec.CommandContext(ctx, "k3d", "registry", "delete", "k3d-"+name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("k3d registry delete k3d-%s: %w\n%s", name, err, out)
	}
	return nil
}

// CreateClusterOpts captures every option `flywheel up` step 7 sets.
type CreateClusterOpts struct {
	Name           string
	K3sImage       string // e.g. v1.34.1-k3s1
	Servers        int    // default 1
	Agents         int    // default 2
	RegistryName   string // wires --registry-use k3d-<name>
	HttpPort       int    // host port → loadbalancer:80
	HttpsPort      int    // host port → loadbalancer:443
	WorkspacesRoot string // mounted at /workspaces on every node
	// GitCommonDir, when set, bind-mounts a git worktree's shared git dir at its
	// own absolute host path so the in-cluster git-deploy-controller can resolve
	// the checkout's objects/refs (issue #62). Empty for a normal clone.
	GitCommonDir string
}

// CreateCluster creates a k3d cluster with the design-mandated topology.
// Idempotent: returns nil if a cluster of this name already exists.
func CreateCluster(ctx context.Context, opts CreateClusterOpts) error {
	if exists, err := clusterExists(ctx, opts.Name); err != nil {
		return err
	} else if exists {
		// A running cluster is a no-op (re-running `up` is idempotent).
		// A *stopped* cluster (e.g. a leftover, or a manual `k3d cluster
		// stop`) is NOT restarted: k3d reassigns container IPs across
		// stop/start, leaving stale Node IPs that break apiserver→kubelet
		// (port-forward/exec/logs). Recreate it fresh instead, which is
		// the only reliable path on the multi-node topology.
		running, err := clusterRunning(ctx, opts.Name)
		if err != nil {
			return err
		}
		if running {
			return nil
		}
		if err := DeleteCluster(ctx, opts.Name); err != nil {
			return err
		}
		// fall through to a clean create
	}

	cmd := exec.CommandContext(ctx, "k3d", clusterCreateArgs(opts)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("k3d cluster create %s: %w\n%s", opts.Name, err, out)
	}
	return nil
}

// clusterCreateArgs builds the `k3d cluster create` argv. Pure (no shell-out)
// so the volume/port wiring is unit-testable. A worktree's shared git dir is
// mounted at its own absolute host path (issue #62) so the in-cluster
// git-deploy-controller can resolve the checkout — only when GitCommonDir is set
// (a normal clone leaves it empty and gets no extra mount).
func clusterCreateArgs(opts CreateClusterOpts) []string {
	args := []string{
		"cluster", "create", opts.Name,
		"--servers", fmt.Sprintf("%d", defaultIfZero(opts.Servers, 1)),
		"--agents", fmt.Sprintf("%d", defaultIfZero(opts.Agents, 2)),
		"--registry-use", "k3d-" + opts.RegistryName,
		"--port", fmt.Sprintf("%d:80@loadbalancer", opts.HttpPort),
		"--port", fmt.Sprintf("%d:443@loadbalancer", opts.HttpsPort),
		"--volume", fmt.Sprintf("%s:/workspaces@agent:*", opts.WorkspacesRoot),
		"--volume", fmt.Sprintf("%s:/workspaces@server:*", opts.WorkspacesRoot),
	}
	if opts.GitCommonDir != "" {
		args = append(args,
			"--volume", fmt.Sprintf("%s:%s@agent:*", opts.GitCommonDir, opts.GitCommonDir),
			"--volume", fmt.Sprintf("%s:%s@server:*", opts.GitCommonDir, opts.GitCommonDir),
		)
	}
	args = append(args, "--kubeconfig-switch-context", "--wait")
	if opts.K3sImage != "" {
		args = append(args, "--image", "rancher/k3s:"+opts.K3sImage)
	}
	return args
}

// DeleteCluster removes the cluster entirely. Idempotent.
func DeleteCluster(ctx context.Context, name string) error {
	if exists, err := clusterExists(ctx, name); err != nil {
		return err
	} else if !exists {
		return nil
	}
	cmd := exec.CommandContext(ctx, "k3d", "cluster", "delete", name)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("k3d cluster delete %s: %w\n%s", name, err, out)
	}
	return nil
}

// KubeContext is the kubeconfig context k3d creates for a cluster. The
// `--kubeconfig-switch-context` flag during create already switches the
// active context to this; helpers return it for explicit `--context`
// usage in dependent commands.
func KubeContext(clusterName string) string {
	return "k3d-" + clusterName
}

// ClusterRunning reports whether the named cluster exists and has at least
// one server node up. Exported for `flywheel up`'s host-port healing, which
// must leave a running cluster's loadbalancer ports untouched (re-running up
// stays idempotent) and only heal ports a foreign process holds.
func ClusterRunning(ctx context.Context, name string) (bool, error) {
	return clusterRunning(ctx, name)
}

// RegistryExists reports whether a k3d-managed registry of this name exists.
// Exported for `flywheel up`'s host-port healing: an existing registry already
// holds registry_port legitimately (CreateRegistry is a no-op for it), so that
// port must not be reallocated out from under it.
func RegistryExists(ctx context.Context, name string) (bool, error) {
	return registryExists(ctx, name)
}

func clusterExists(ctx context.Context, name string) (bool, error) {
	cmd := exec.CommandContext(ctx, "k3d", "cluster", "list", "--no-headers")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("k3d cluster list: %w\n%s", err, out)
	}
	for _, line := range splitLines(string(out)) {
		if firstField(line) == name {
			return true, nil
		}
	}
	return false, nil
}

// clusterRunning reports whether the cluster's server node is up. k3d's
// `cluster list --no-headers` SERVERS column reads "<running>/<total>"
// (e.g. "1/1" running, "0/1" stopped); the cluster is running when at
// least one server is up.
func clusterRunning(ctx context.Context, name string) (bool, error) {
	cmd := exec.CommandContext(ctx, "k3d", "cluster", "list", "--no-headers")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("k3d cluster list: %w\n%s", err, out)
	}
	for _, line := range splitLines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == name {
			servers := fields[1] // "<running>/<total>"
			return !strings.HasPrefix(servers, "0/"), nil
		}
	}
	return false, nil
}

func registryExists(ctx context.Context, name string) (bool, error) {
	cmd := exec.CommandContext(ctx, "k3d", "registry", "list", "--no-headers")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("k3d registry list: %w\n%s", err, out)
	}
	// k3d prefixes registry names with `k3d-` in the list output; check
	// both the bare name (in case it's already there) and the prefixed
	// form.
	want := []string{name, "k3d-" + name}
	for _, line := range splitLines(string(out)) {
		first := firstField(line)
		for _, w := range want {
			if first == w {
				return true, nil
			}
		}
	}
	return false, nil
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func firstField(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' || s[i] == '\t' {
			return s[:i]
		}
	}
	return s
}

func defaultIfZero(v, d int) int {
	if v == 0 {
		return d
	}
	return v
}
