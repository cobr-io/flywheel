package imagepin

import (
	"context"
	"io"
	"strings"
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

func TestHasRegistryHost(t *testing.T) {
	for _, c := range []struct {
		ref  string
		want bool
	}{
		{"flywheel-dev/git-server:dogfood", false}, // make-images build: no registry host
		{"local/git-server:dev", false},            // Docker Hub-style org/name
		{"git-server:dogfood", false},              // bare library name
		{"ghcr.io/cobr-io/git-server:v0.1.0", true},
		{"localhost:5000/git-server:tag", true},
		{"registry:5000/git-server", true}, // host with a port, no dot
		{"my.registry/team/img:tag", true}, // host with a dot
	} {
		if got := hasRegistryHost(c.ref); got != c.want {
			t.Errorf("hasRegistryHost(%q) = %v, want %v", c.ref, got, c.want)
		}
	}
}

func TestIsLocalOnlyOverride(t *testing.T) {
	// The default ghcr ref is never local-only (it's pulled from a registry).
	if isLocalOnlyOverride("git-server", "v0.1.0", DefaultRef("git-server", "v0.1.0")) {
		t.Error("default ref misclassified as a local-only override")
	}
	// A registry-qualified override is pullable, not local-only.
	if isLocalOnlyOverride("git-server", "v0.1.0", "ghcr.io/me/git-server:wip") {
		t.Error("registry-qualified override misclassified as local-only")
	}
	// The documented dogfood pin is local-only.
	if !isLocalOnlyOverride("git-server", "v0.1.0", "flywheel-dev/git-server:dogfood") {
		t.Error("flywheel-dev dogfood pin should be a local-only override")
	}
}

// CheckLocalOverrides flags only the local-only dogfood pins that are absent
// from the docker store — skipping defaults, registry-qualified overrides, and
// already-built images.
func TestCheckLocalOverrides(t *testing.T) {
	cfg := &schema.File{}
	cfg.Flywheel.Version = "v0.1.0"
	cfg.Flywheel.Images = map[string]string{
		"git-server":               "flywheel-dev/git-server:dogfood",    // local-only, present below
		"git-auto-sync":            "flywheel-dev/git-auto-sync:dogfood", // local-only, MISSING
		"image-builder-controller": "ghcr.io/me/ibc:wip",                 // registry override, skipped
	}
	// Stub the docker probe: only git-server's ref is "built".
	orig := inLocalDocker
	t.Cleanup(func() { inLocalDocker = orig })
	inLocalDocker = func(_ context.Context, ref string) bool {
		return ref == "flywheel-dev/git-server:dogfood"
	}

	missing := CheckLocalOverrides(context.Background(), cfg)
	if len(missing) != 1 {
		t.Fatalf("want exactly 1 missing override, got %d: %+v", len(missing), missing)
	}
	if missing[0].Name != "git-auto-sync" || missing[0].Ref != "flywheel-dev/git-auto-sync:dogfood" {
		t.Errorf("unexpected missing entry: %+v", missing[0])
	}
}

// All defaults (no overrides) → nothing flagged, and the docker probe is never
// even consulted for released refs.
func TestCheckLocalOverrides_DefaultsClean(t *testing.T) {
	cfg := &schema.File{}
	cfg.Flywheel.Version = "v0.1.0"
	orig := inLocalDocker
	t.Cleanup(func() { inLocalDocker = orig })
	inLocalDocker = func(context.Context, string) bool {
		t.Fatal("inLocalDocker should not be probed for default refs")
		return false
	}
	if missing := CheckLocalOverrides(context.Background(), cfg); len(missing) != 0 {
		t.Errorf("defaults should flag nothing, got %+v", missing)
	}
}

func TestMissingDogfoodError(t *testing.T) {
	err := MissingDogfoodError([]MissingDogfood{
		{Name: "git-auto-sync", Ref: "flywheel-dev/git-auto-sync:dogfood"},
	})
	msg := err.Error()
	for _, want := range []string{"git-auto-sync", "flywheel-dev/git-auto-sync:dogfood", "make images", "docs/dev/dogfood.md"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
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
