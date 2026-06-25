package imagepin

import (
	"context"
	"io"
	"testing"

	"github.com/cobr-io/flywheel/internal/cli/schema"
)

func TestResolve_DefaultsWhenNoOverrides(t *testing.T) {
	cfg := &schema.File{}
	cfg.Flywheel.Version = "v0.1.0"
	refs := Resolve(cfg)
	if refs["git-server"] != "ghcr.io/cobr-io/git-server:v0.1.0" {
		t.Errorf("git-server default = %q", refs["git-server"])
	}
	if refs["git-auto-sync"] != "ghcr.io/cobr-io/git-auto-sync:v0.1.0" {
		t.Errorf("git-auto-sync default = %q", refs["git-auto-sync"])
	}
	if refs["image-builder-controller"] != "ghcr.io/cobr-io/image-builder-controller:v0.1.0" {
		t.Errorf("image-builder-controller default = %q", refs["image-builder-controller"])
	}
}

func TestResolve_OverridesWin(t *testing.T) {
	cfg := &schema.File{}
	cfg.Flywheel.Version = "v0.1.0"
	cfg.Flywheel.Images = map[string]string{
		"git-server": "local/git-server:dev",
	}
	refs := Resolve(cfg)
	// An override is returned verbatim (digest content-addressing happens at
	// deploy time, not here).
	if refs["git-server"] != "local/git-server:dev" {
		t.Errorf("override ignored: %q", refs["git-server"])
	}
	// Other two still defaults.
	if refs["git-auto-sync"] != "ghcr.io/cobr-io/git-auto-sync:v0.1.0" {
		t.Errorf("git-auto-sync should fall back to default when not overridden, got %q", refs["git-auto-sync"])
	}
}

func TestIsDefault(t *testing.T) {
	if !IsDefault("git-server", "v0.1.0", "ghcr.io/cobr-io/git-server:v0.1.0") {
		t.Error("default ref not detected")
	}
	if IsDefault("git-server", "v0.1.0", "local/git-server:dev") {
		t.Error("custom ref misdetected as default")
	}
}

func TestRegistryRefs(t *testing.T) {
	// registryRefs takes the final tag verbatim (dogfood-<sha> or a version).
	push, pull := registryRefs("acme-local-registry", 50001, "git-auto-sync", "dogfood-abcdef012345")
	if push != "localhost:50001/git-auto-sync:dogfood-abcdef012345" {
		t.Errorf("push ref = %q", push)
	}
	if pull != "k3d-acme-local-registry:5000/git-auto-sync:dogfood-abcdef012345" {
		t.Errorf("pull ref = %q", pull)
	}
	// A released image is served under its immutable version tag.
	push, pull = registryRefs("acme-local-registry", 50001, "git-server", "v0.1.0")
	if push != "localhost:50001/git-server:v0.1.0" {
		t.Errorf("versioned push ref = %q", push)
	}
	if pull != "k3d-acme-local-registry:5000/git-server:v0.1.0" {
		t.Errorf("versioned pull ref = %q", pull)
	}
}

func TestDogfoodTag(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"sha256:abcdef0123456789", "dogfood-abcdef012345"},
		{"abcdef0123456789", "dogfood-abcdef012345"},
		{"sha256:abc", "dogfood-abc"},
	} {
		if got := dogfoodTag(c.in); got != c.want {
			t.Errorf("dogfoodTag(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A default ref in add-app resolves to the local-registry pull ref (`up` has
// already mirrored it there) — computed with no docker work.
func TestEnsureInRegistry_DefaultServedFromRegistry(t *testing.T) {
	ref := DefaultRef("git-auto-sync", "v0.1.0")
	got, err := EnsureInRegistry(context.Background(), ref, "acme-local-registry", 50001, "git-auto-sync", "v0.1.0", io.Discard)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "k3d-acme-local-registry:5000/git-auto-sync:v0.1.0"
	if got != want {
		t.Errorf("default ref should resolve to the registry pull ref: got %q want %q", got, want)
	}
}
