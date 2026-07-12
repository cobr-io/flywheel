package imagepin

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestResolve_DefaultsWhenNoOverrides(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	if !IsDefault("git-server", "v0.1.0", "ghcr.io/cobr-io/git-server:v0.1.0") {
		t.Error("default ref not detected")
	}
	if IsDefault("git-server", "v0.1.0", "local/git-server:dev") {
		t.Error("custom ref misdetected as default")
	}
}

func TestRegistryRefs(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
// already-built images. Exercised through the unexported checkLocalOverrides
// so the docker probe is injected via deps, not a package-level var — no
// shared state with other tests, so this is safe under t.Parallel.
func TestCheckLocalOverrides(t *testing.T) {
	t.Parallel()
	cfg := &schema.File{}
	cfg.Flywheel.Version = "v0.1.0"
	cfg.Flywheel.Images = map[string]string{
		"git-server":               "flywheel-dev/git-server:dogfood",    // local-only, present below
		"git-auto-sync":            "flywheel-dev/git-auto-sync:dogfood", // local-only, MISSING
		"image-builder-controller": "ghcr.io/me/ibc:wip",                 // registry override, skipped
	}
	d := defaultDeps()
	d.inLocalDocker = func(_ context.Context, ref string) bool {
		return ref == "flywheel-dev/git-server:dogfood"
	}

	missing := checkLocalOverrides(context.Background(), d, cfg)
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
	t.Parallel()
	cfg := &schema.File{}
	cfg.Flywheel.Version = "v0.1.0"
	d := defaultDeps()
	d.inLocalDocker = func(context.Context, string) bool {
		t.Fatal("inLocalDocker should not be probed for default refs")
		return false
	}
	if missing := checkLocalOverrides(context.Background(), d, cfg); len(missing) != 0 {
		t.Errorf("defaults should flag nothing, got %+v", missing)
	}
}

func TestMissingDogfoodError(t *testing.T) {
	t.Parallel()
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

// The default ref is tagged with its immutable version — the digest source is
// never consulted (it IS the content address for a release).
func TestPickTag_DefaultUsesVersion(t *testing.T) {
	t.Parallel()
	called := false
	tag, err := pickTag(DefaultRef("git-server", "v0.1.0"), "git-server", "v0.1.0", func() (string, error) {
		called = true
		return "sha256:deadbeef", nil
	})
	if err != nil {
		t.Fatalf("pickTag: %v", err)
	}
	if tag != "v0.1.0" {
		t.Errorf("tag = %q, want v0.1.0", tag)
	}
	if called {
		t.Error("digest source should not be consulted for a released ref")
	}
}

// A non-default ref (registry-qualified override or local docker build) is
// content-addressed by whatever digest the caller's source supplies, so the
// tag rolls when that content changes. This is the one tag picker both
// mirrorRemote (registry digest) and mirrorLocal (docker image ID) funnel
// through.
func TestPickTag_OverrideUsesDigestSource(t *testing.T) {
	t.Parallel()
	tag, err := pickTag("ghcr.io/me/git-server:wip", "git-server", "v0.1.0", func() (string, error) {
		return "sha256:abcdef0123456789ffff", nil
	})
	if err != nil {
		t.Fatalf("pickTag: %v", err)
	}
	if tag != "dogfood-abcdef012345" {
		t.Errorf("tag = %q, want dogfood-abcdef012345", tag)
	}
}

// A digest-source failure (e.g. `docker inspect` erroring) is wrapped, not
// swallowed.
func TestPickTag_DigestSourceErrorWrapped(t *testing.T) {
	t.Parallel()
	cause := errors.New("inspect failed")
	_, err := pickTag("ghcr.io/me/git-server:wip", "git-server", "v0.1.0", func() (string, error) {
		return "", cause
	})
	if !errors.Is(err, cause) {
		t.Errorf("want wrapped cause, got: %v", err)
	}
}

// A registry-hosted ref is copied registry→registry: remoteImage reads the
// host-platform image and remoteWrite streams it to the local registry under
// the version tag. The docker store is never touched.
func TestMirrorToRegistry_RegistryRefStreamsToLocalRegistry(t *testing.T) {
	t.Parallel()
	img, err := random.Image(1024, 1)
	if err != nil {
		t.Fatalf("random.Image: %v", err)
	}
	var gotPlatform v1.Platform
	var gotDst string
	d := defaultDeps()
	// The copy platform must come from the docker DAEMON arch, not the CLI
	// binary's GOARCH (issue #54) — stub it to a fixed value and prove it flows
	// through to the go-containerregistry copy.
	d.daemonArch = func(context.Context) (string, error) { return "amd64", nil }
	d.remoteImage = func(_ context.Context, ref name.Reference, p v1.Platform) (v1.Image, error) {
		gotPlatform = p
		if ref.Name() != "ghcr.io/cobr-io/git-server:v0.1.0" {
			t.Errorf("source ref = %q", ref.Name())
		}
		return img, nil
	}
	d.remoteWrite = func(_ context.Context, dst name.Reference, _ v1.Image) error {
		gotDst = dst.Name()
		return nil
	}
	// A registry ref must never fall back to the docker store.
	d.inLocalDocker = func(context.Context, string) bool {
		t.Fatal("inLocalDocker must not be probed for a registry-hosted ref")
		return false
	}

	pullRef, err := mirrorToRegistry(context.Background(), d,
		DefaultRef("git-server", "v0.1.0"), "acme-local-registry", 50001, "git-server", "v0.1.0", io.Discard)
	if err != nil {
		t.Fatalf("mirrorToRegistry: %v", err)
	}
	if pullRef != "k3d-acme-local-registry:5000/git-server:v0.1.0" {
		t.Errorf("pull ref = %q", pullRef)
	}
	if gotDst != "localhost:50001/git-server:v0.1.0" {
		t.Errorf("push dst = %q", gotDst)
	}
	if gotPlatform.OS != "linux" || gotPlatform.Architecture != "amd64" {
		t.Errorf("copy platform = %+v, want linux/amd64 (the stubbed daemon arch)", gotPlatform)
	}
}

// hostPlatform derives the arch from the docker daemon (the k3d nodes run on it),
// not the CLI binary's GOARCH — so a cross-arch install mirrors the arch the
// cluster can actually exec (issue #54).
func TestHostPlatform_UsesDaemonArch(t *testing.T) {
	t.Parallel()
	d := defaultDeps()
	d.daemonArch = func(context.Context) (string, error) { return "amd64", nil }
	p := hostPlatform(context.Background(), d)
	if p.OS != "linux" || p.Architecture != "amd64" {
		t.Errorf("hostPlatform = %+v, want linux/amd64", p)
	}
}

// When the daemon can't be queried (docker down/wedged), hostPlatform falls back
// to the binary's GOARCH — the pre-#54 behaviour, and harmless since the
// subsequent registry read fails on a dead daemon anyway.
func TestHostPlatform_FallsBackToGOARCHOnDaemonError(t *testing.T) {
	t.Parallel()
	d := defaultDeps()
	d.daemonArch = func(context.Context) (string, error) {
		return "", errors.New("docker daemon not reachable")
	}
	p := hostPlatform(context.Background(), d)
	if p.OS != "linux" || p.Architecture != runtime.GOARCH {
		t.Errorf("hostPlatform = %+v, want linux/%s (GOARCH fallback)", p, runtime.GOARCH)
	}
}

// A local-only dogfood ref names no registry, so it must take the docker path,
// not the registry→registry copy. When it is not yet built, the mirror stops
// with build guidance and never reaches remoteImage.
func TestMirrorToRegistry_LocalOnlyRefUsesDockerPath(t *testing.T) {
	t.Parallel()
	d := defaultDeps()
	d.remoteImage = func(_ context.Context, _ name.Reference, _ v1.Platform) (v1.Image, error) {
		t.Fatal("remoteImage must not be called for a local-only dogfood ref")
		return nil, nil
	}
	d.inLocalDocker = func(context.Context, string) bool { return false }

	_, err := mirrorToRegistry(context.Background(), d,
		"flywheel-dev/git-server:dogfood", "acme-local-registry", 50001, "git-server", "v0.1.0", io.Discard)
	if err == nil {
		t.Fatal("want MissingDogfoodError for an unbuilt local-only ref")
	}
	if !strings.Contains(err.Error(), "make images") {
		t.Errorf("error should be the dogfood build guidance, got: %v", err)
	}
}

// A read failure on the unmodified default ref surfaces the option-(c) override
// guidance, with the underlying cause preserved for diagnosis.
func TestMirrorRemote_DefaultRefReadFailureReturnsOptionC(t *testing.T) {
	t.Parallel()
	cause := errors.New("MANIFEST_UNKNOWN: manifest unknown")
	d := defaultDeps()
	d.remoteImage = func(_ context.Context, _ name.Reference, _ v1.Platform) (v1.Image, error) {
		return nil, cause
	}
	_, err := mirrorRemote(context.Background(), d,
		DefaultRef("git-server", "v0.1.0"), "acme-local-registry", 50001, "git-server", "v0.1.0", io.Discard)
	if err == nil {
		t.Fatal("want an error on read failure")
	}
	if !strings.Contains(err.Error(), "flywheel.yaml.local") {
		t.Errorf("expected option-(c) override guidance, got: %v", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("underlying cause should be wrapped for diagnosis, got: %v", err)
	}
}

// End-to-end against a real in-memory registry (no stubbed remoteImage/
// remoteWrite): the whole point of issue #50 is that copying a multi-arch
// index must land a SINGLE-platform image in the local registry, never an
// index (which `docker push` rejects under the containerd image store). This
// proves that property through the actual go-containerregistry copy path.
func TestMirrorRemote_ResolvesIndexToSinglePlatformImage(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	// httptest listens on a loopback IP, which go-containerregistry serves over
	// plain HTTP — one server stands in for both ghcr (source) and k3d (dest).
	host := u.Host
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	// Seed a multi-arch index (linux/amd64 + linux/arm64) as the "released" image.
	idx := mutate.IndexMediaType(empty.Index, types.OCIImageIndex)
	childByArch := map[string]v1.Hash{}
	for _, arch := range []string{"amd64", "arm64"} {
		img, err := random.Image(1024, 1)
		if err != nil {
			t.Fatal(err)
		}
		d, err := img.Digest()
		if err != nil {
			t.Fatal(err)
		}
		childByArch[arch] = d
		idx = mutate.AppendManifests(idx, mutate.IndexAddendum{
			Add:        img,
			Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: arch}},
		})
	}
	// Pin the copy platform to the test's GOARCH so the assertion is
	// independent of whatever docker daemon (if any) backs the CI runner.
	d := defaultDeps()
	d.daemonArch = func(context.Context) (string, error) { return runtime.GOARCH, nil }
	wantChild, ok := childByArch[runtime.GOARCH]
	if !ok {
		t.Skipf("test host arch %q not represented in the seed index", runtime.GOARCH)
	}
	srcRef, err := name.ParseReference(host + "/cobr-io/git-server:v9.9.9")
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(srcRef, idx); err != nil {
		t.Fatalf("seed source index: %v", err)
	}

	// Mirror it. The ref is registry-hosted but not the default ghcr ref, so the
	// local tag is content-addressed by the resolved image's digest.
	pullRef, err := mirrorRemote(context.Background(), d,
		host+"/cobr-io/git-server:v9.9.9", "test-registry", port, "git-server", "v0.1.0", io.Discard)
	if err != nil {
		t.Fatalf("mirrorRemote: %v", err)
	}
	wantTag := dogfoodTag(wantChild.String())
	if wantPull := "k3d-test-registry:5000/git-server:" + wantTag; pullRef != wantPull {
		t.Errorf("pull ref = %q, want %q", pullRef, wantPull)
	}

	// Read the mirrored artifact back: it must be a single image manifest — NOT
	// an index — and exactly the host-arch child of the source index.
	dstRef, err := name.ParseReference("localhost:"+u.Port()+"/git-server:"+wantTag, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := remote.Get(dstRef)
	if err != nil {
		t.Fatalf("read back mirrored image: %v", err)
	}
	if desc.MediaType.IsIndex() {
		t.Fatalf("mirrored a multi-arch index (containerd-unsafe); media type = %s", desc.MediaType)
	}
	if !desc.MediaType.IsImage() {
		t.Fatalf("mirrored artifact is not an image manifest; media type = %s", desc.MediaType)
	}
	if desc.Digest != wantChild {
		t.Errorf("mirrored digest = %s, want host-arch child %s", desc.Digest, wantChild)
	}
}
