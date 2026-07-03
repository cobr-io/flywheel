package k3d

import (
	"strings"
	"testing"
)

func joined(args []string) string { return strings.Join(args, " ") }

func TestClusterCreateArgs_NormalClone_NoGitDirMount(t *testing.T) {
	args := clusterCreateArgs(CreateClusterOpts{
		Name:           "acme-local",
		RegistryName:   "acme-local-registry",
		HttpPort:       8080,
		HttpsPort:      8540,
		WorkspacesRoot: "/Users/x/src",
	})
	got := joined(args)

	// The workspaces mount is always present, on both node roles.
	for _, want := range []string{
		"/Users/x/src:/workspaces@agent:*",
		"/Users/x/src:/workspaces@server:*",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing workspaces volume %q in: %s", want, got)
		}
	}
	// No GitCommonDir → no extra host-abs mount.
	if strings.Contains(got, "@agent:*") && strings.Count(got, "--volume") != 2 {
		t.Errorf("expected exactly 2 --volume flags for a clone, got: %s", got)
	}
}

func TestClusterCreateArgs_Worktree_MountsGitCommonDirAtAbsPath(t *testing.T) {
	common := "/Users/x/src/acme/.git"
	args := clusterCreateArgs(CreateClusterOpts{
		Name:           "acme-local",
		RegistryName:   "acme-local-registry",
		HttpPort:       8080,
		HttpsPort:      8540,
		WorkspacesRoot: "/Users/x/src",
		GitCommonDir:   common,
	})
	got := joined(args)

	// The shared git dir is bind-mounted at its OWN absolute path (host==container)
	// on both node roles, so the worktree's `.git` gitdir pointer resolves.
	for _, want := range []string{
		common + ":" + common + "@agent:*",
		common + ":" + common + "@server:*",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing git-common-dir volume %q in: %s", want, got)
		}
	}
	if n := strings.Count(got, "--volume"); n != 4 {
		t.Errorf("expected 4 --volume flags (workspaces + git-dir, both roles), got %d: %s", n, got)
	}
}

func TestClusterCreateArgs_ImageAppendedWhenSet(t *testing.T) {
	args := clusterCreateArgs(CreateClusterOpts{Name: "c", RegistryName: "r", K3sImage: "v1.34.1-k3s1"})
	if !strings.Contains(joined(args), "--image rancher/k3s:v1.34.1-k3s1") {
		t.Errorf("--image not wired: %s", joined(args))
	}
	// Absent when unset.
	bare := clusterCreateArgs(CreateClusterOpts{Name: "c", RegistryName: "r"})
	if strings.Contains(joined(bare), "--image") {
		t.Errorf("--image should be absent when K3sImage empty: %s", joined(bare))
	}
}
