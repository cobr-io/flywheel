// Package imagepin resolves the four Flywheel container image references
// (git-server, git-auto-sync, image-builder-controller, git-deploy-controller —
// canonically schema.ImageNames) from a client's config and makes sure each is
// reachable by every node in the k3d cluster at `flywheel up` time.
//
// Resolution: `cfg.Flywheel.Images.<name>` if set, else the public
// default `ghcr.io/cobr-io/<name>:<flywheel.version>`. The natural
// home for per-developer dogfood overrides is `flywheel.yaml.local`
// (gitignored, deep-merged).
//
// Loading strategy: every image — released or dogfood — is mirrored into
// the cluster's LOCAL registry and referenced by its in-cluster registry
// path (`k3d-<registry>:5000/<name>:<tag>`). A registry-served image is
// pull-on-demand from every node, so it can't go missing on a node through
// scheduling, GC eviction, or the add-app-after-up gap (issue #14).
//
// The mirror splits on whether the source ref names a registry:
//
//   - Registry-hosted ref (the default `ghcr.io/cobr-io/<name>:<version>`, or
//     any registry-qualified override): copied registry→registry with
//     go-containerregistry, scoped to the host platform (`linux/<GOARCH>`).
//     This streams a single-arch image straight into the local registry
//     without a docker-store round-trip. Crucially it never `docker tag`s or
//     `docker push`es a multi-arch manifest index, which fails under Docker's
//     containerd image store (`does not provide any platform`, issue #50);
//     the released images are multi-arch, so that path was broken there.
//     Tagging: the immutable `:<version>` for the default ref (the version IS
//     the content address for a release), or a content-addressed
//     `:dogfood-<sha>` derived from the source image digest for an override.
//     If reading the default ref fails and no override is set, return
//     option (c) — a clear error naming the override stanza for
//     `flywheel.yaml.local`.
//
//   - Local-only dogfood ref (`flywheel-dev/<name>:dogfood`, naming no
//     registry): these exist only in the host docker store (a `make images`
//     build) and can't be read from a registry, so they keep the docker
//     `tag`+`push` path. They are single-arch, so the containerd-store index
//     bug never applies. Tagged `:dogfood-<sha>` from the local image ID; the
//     sha suffix forces a re-pull on change so a bare `IfNotPresent` node
//     never serves a stale image off the mutable `:dogfood` tag.
package imagepin

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"runtime"
	"strings"

	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/style"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// inClusterRegistryPort is the port the k3d registry *container* listens
// on inside the cluster network — always 5000 regardless of the host-side
// published port (cfg.Cluster.RegistryPort). In-cluster image pulls hit
// this port; the host-side port is only for the `docker push` from the
// developer's machine. Canonical definition: internal/controller.Config's
// InClusterRegistryPort (duplicated here to avoid importing the controller
// package into the CLI).
const inClusterRegistryPort = "5000"

// DefaultRef returns the public ghcr.io reference for an image at the
// client's pinned Flywheel version. The version IS the content address for
// released images (immutable per release).
func DefaultRef(name, version string) string {
	return fmt.Sprintf("ghcr.io/cobr-io/%s:%s", name, version)
}

// Resolve returns the map of image name → resolved ref for the known images
// (schema.ImageNames), honouring `cfg.Flywheel.Images` overrides. A bare `:dogfood`
// override is returned verbatim here; `flywheel up` content-addresses it at
// deploy time by the local image's digest (`:dogfood-<imageID>`), so a
// rebuilt image rolls the Deployment without a manual pod recreate.
func Resolve(cfg *schema.File) map[string]string {
	out := make(map[string]string, len(schema.ImageNames))
	for _, name := range schema.ImageNames {
		if ref, ok := cfg.Flywheel.Images[name]; ok && ref != "" {
			out[name] = ref
		} else {
			out[name] = DefaultRef(name, cfg.Flywheel.Version)
		}
	}
	return out
}

// IsDefault reports whether `ref` is the default ghcr.io reference for
// `name` at `version`. Gates option (c) on a pull failure and selects the
// registry tag scheme (immutable `:<version>` for a release vs
// content-addressed `:dogfood-<sha>` for an override).
func IsDefault(name, version, ref string) bool {
	return ref == DefaultRef(name, version)
}

// hasRegistryHost reports whether `ref` names an explicit registry that a
// `docker pull` could resolve. Docker treats the component before the first
// '/' as a registry host only when it contains '.' or ':' or is exactly
// "localhost"; otherwise the ref is an implicit Docker Hub name. A dogfood
// image built by `make images` (`flywheel-dev/<name>:dogfood`) has no registry
// host — it lives only in the local docker store, so pulling it is doomed.
func hasRegistryHost(ref string) bool {
	host, _, ok := strings.Cut(ref, "/")
	if !ok {
		return false // bare "name:tag" — a Docker Hub library image
	}
	return strings.ContainsAny(host, ".:") || host == "localhost"
}

// isLocalOnlyOverride reports whether `ref` is an override that can only come
// from a local build: non-default AND naming no registry. These are exactly
// the refs `make images` produces (`flywheel-dev/<name>:<tag>`). A missing one
// can't be pulled, so `up`/`add app` must stop with build guidance rather than
// attempt a doomed pull.
func isLocalOnlyOverride(name, version, ref string) bool {
	return !IsDefault(name, version, ref) && !hasRegistryHost(ref)
}

// MissingDogfood is one dogfood override that's pinned but absent from the host
// docker store and un-pullable (it names no registry).
type MissingDogfood struct {
	Name string // image name, one of schema.ImageNames
	Ref  string // the pinned override ref, e.g. flywheel-dev/git-server:dogfood
}

// CheckLocalOverrides probes every resolved image and returns the dogfood
// overrides that can't be satisfied locally: a local-only override (per
// isLocalOnlyOverride) that is not present in the host docker store. Released
// (default) refs and registry-qualified overrides are skipped — those are
// pulled on demand by the mirror step. The result follows schema.ImageNames
// order for a stable report, and is empty when every override is buildable.
func CheckLocalOverrides(ctx context.Context, cfg *schema.File) []MissingDogfood {
	resolved := Resolve(cfg)
	var missing []MissingDogfood
	for _, name := range schema.ImageNames {
		ref := resolved[name]
		if !isLocalOnlyOverride(name, cfg.Flywheel.Version, ref) {
			continue
		}
		if inLocalDocker(ctx, ref) {
			continue // already built — nothing to warn about
		}
		missing = append(missing, MissingDogfood{Name: name, Ref: ref})
	}
	return missing
}

// MissingDogfoodError renders the actionable "build them first" message for the
// dogfood overrides that aren't in the local docker store. Used by `up`'s
// pre-flight (one message for all missing images) and by ensureLocal's
// point-of-use guard (a single-element slice).
func MissingDogfoodError(missing []MissingDogfood) error {
	w := 0
	for _, m := range missing {
		if len(m.Name) > w {
			w = len(m.Name)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "dogfood image(s) not found in your local docker store:\n\n")
	for _, m := range missing {
		fmt.Fprintf(&b, "  %-*s  %s\n", w, m.Name, m.Ref)
	}
	b.WriteString(`
These refs name no registry, so flywheel up can't pull them — they only exist
once you build them from the Flywheel source. Build them, then re-run:

  cd <your flywheel checkout>
  make images
  flywheel up

(Overrides are pinned in flywheel.yaml.local under flywheel.images.*;
see docs/dev/dogfood.md.)`)
	return fmt.Errorf("%s", b.String())
}

// EnsureInCluster mirrors `ref` into the cluster's local registry — for both
// released and dogfood images — and returns the in-cluster pull ref the
// rendered manifests should use (`k3d-<registry>:5000/<name>:<tag>`).
func EnsureInCluster(ctx context.Context, ref, registryName string, registryPort int, imageName, version string, stdout io.Writer) (string, error) {
	// No progress markers here — callers wrap this in style.Spin, which
	// owns the line. Verbose subprocess output still flows via
	// style.VerboseWriter inside the helpers.
	return mirrorToRegistry(ctx, ref, registryName, registryPort, imageName, version, stdout)
}

// mirrorToRegistry copies `ref` into the cluster's local registry and returns
// the in-cluster pull ref. A registry-hosted ref is streamed registry→registry
// scoped to the host platform (containerd-store-safe, issue #50); a local-only
// dogfood ref is tag+pushed from the host docker store where it was built.
func mirrorToRegistry(ctx context.Context, ref, registryName string, registryPort int, imageName, version string, stdout io.Writer) (string, error) {
	if hasRegistryHost(ref) {
		return mirrorRemote(ctx, ref, registryName, registryPort, imageName, version, stdout)
	}
	return mirrorLocal(ctx, ref, registryName, registryPort, imageName, version, stdout)
}

// hostPlatform is the single platform the local k3d cluster runs: `linux` on
// the flywheel binary's arch. On every supported host the binary arch matches
// the docker-VM/node arch (arm64 on Apple Silicon, amd64 on Intel), so scoping
// the copy to it moves only the bytes the cluster can actually run.
func hostPlatform() v1.Platform {
	return v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
}

// mirrorRemote copies a registry-hosted `ref` (the default ghcr ref or a
// registry-qualified override) into the local registry with go-containerregistry,
// selecting the host platform out of a multi-arch index and streaming it in
// without touching the host docker store. This is the containerd-image-store-safe
// path: it never `docker push`es a manifest index (issue #50).
func mirrorRemote(ctx context.Context, ref, registryName string, registryPort int, imageName, version string, stdout io.Writer) (string, error) {
	srcRef, err := name.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("parse source ref %s: %w", ref, err)
	}
	platform := hostPlatform()
	img, err := remoteImage(ctx, srcRef, platform)
	if err != nil {
		// A read failure on the unmodified default ref means no published
		// release is reachable — surface the option-(c) override guidance.
		// The underlying cause (auth, 404, no matching platform) rides along
		// via %w, so the failure stays diagnosable (issue #50 secondary ask).
		if IsDefault(imageName, version, ref) {
			return "", optionCError(imageName, version, ref, err)
		}
		return "", fmt.Errorf("read %s: %w", ref, err)
	}
	tag, err := remoteTag(img, ref, imageName, version)
	if err != nil {
		return "", err
	}
	pushRef, pullRef := registryRefs(registryName, registryPort, imageName, tag)
	// name.Insecure lets the copy speak plain HTTP to the k3d registry (it is
	// also localhost, which go-containerregistry treats as insecure anyway).
	dstRef, err := name.ParseReference(pushRef, name.Insecure)
	if err != nil {
		return "", fmt.Errorf("parse destination ref %s: %w", pushRef, err)
	}
	fmt.Fprintf(style.VerboseWriter(stdout), "copy %s → %s (platform %s/%s)\n", ref, pushRef, platform.OS, platform.Architecture)
	if err := remoteWrite(ctx, dstRef, img); err != nil {
		return "", fmt.Errorf("push %s: %w", pushRef, err)
	}
	return pullRef, nil
}

// mirrorLocal handles a local-only dogfood ref (naming no registry): it lives
// only in the host docker store, so it is tag+pushed from there. These builds
// are single-arch, so the containerd-store index push bug never applies.
func mirrorLocal(ctx context.Context, ref, registryName string, registryPort int, imageName, version string, stdout io.Writer) (string, error) {
	if err := ensureLocal(ctx, ref, imageName); err != nil {
		return "", err
	}
	tag, err := registryTag(ctx, ref, imageName, version)
	if err != nil {
		return "", err
	}
	pushRef, pullRef := registryRefs(registryName, registryPort, imageName, tag)
	if err := dockerTag(ctx, ref, pushRef, stdout); err != nil {
		return "", fmt.Errorf("tag %s as %s: %w", ref, pushRef, err)
	}
	if err := dockerPush(ctx, pushRef, stdout); err != nil {
		return "", fmt.Errorf("push %s: %w", pushRef, err)
	}
	return pullRef, nil
}

// remoteImage reads `ref` from its registry as a single-platform image; when
// the ref is a multi-arch index, `platform` selects the matching manifest. A
// package var so tests can stub the network dependency. Auth follows the docker
// keychain (`docker login`), falling back to anonymous for public images.
var remoteImage = func(ctx context.Context, ref name.Reference, platform v1.Platform) (v1.Image, error) {
	return remote.Image(ref,
		remote.WithContext(ctx),
		remote.WithPlatform(platform),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	)
}

// remoteWrite pushes `img` to `dst` (the local k3d registry). A package var so
// tests can stub the network dependency.
var remoteWrite = func(ctx context.Context, dst name.Reference, img v1.Image) error {
	return remote.Write(dst, img,
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.DefaultKeychain),
	)
}

// remoteTag picks the local-registry tag for a registry-hosted image: the
// immutable `:<version>` for the default ref, or a content-addressed
// `:dogfood-<sha>` from the source image's digest for an override (the source
// digest is a stable content address, so the tag rolls when content changes).
func remoteTag(img digester, ref, imageName, version string) (string, error) {
	if IsDefault(imageName, version, ref) {
		return version, nil
	}
	d, err := img.Digest()
	if err != nil {
		return "", fmt.Errorf("digest %s: %w", ref, err)
	}
	return dogfoodTag(d.String()), nil
}

// digester is the slice of v1.Image remoteTag needs — narrowed to one method so
// tests can supply a trivial fake instead of a full image.
type digester interface {
	Digest() (v1.Hash, error)
}

// registryTag picks the local-registry tag for an image: the immutable
// `:<version>` for a released ghcr ref (the version IS its content address), or
// a content-addressed `:dogfood-<sha>` from the local image ID for an override
// (the `:dogfood` tag is mutable, so the sha forces a re-pull on change).
func registryTag(ctx context.Context, ref, imageName, version string) (string, error) {
	if IsDefault(imageName, version, ref) {
		return version, nil
	}
	id, err := imageContentID(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", ref, err)
	}
	return dogfoodTag(id), nil
}

// registryRefs computes the host-side push ref and in-cluster pull ref for an
// image at `tag`:
//
//	push: localhost:<registryPort>/<name>:<tag>   (developer host)
//	pull: k3d-<registry>:5000/<name>:<tag>        (cluster nodes)
//
// Both name the same blob in the same registry container; only the network
// path differs (published host port vs in-cluster DNS name).
func registryRefs(registryName string, registryPort int, imageName, tag string) (push, pull string) {
	push = fmt.Sprintf("localhost:%d/%s:%s", registryPort, imageName, tag)
	pull = fmt.Sprintf("k3d-%s:%s/%s:%s", registryName, inClusterRegistryPort, imageName, tag)
	return push, pull
}

// dogfoodTag derives the content-addressed tag from a content ID
// (`sha256:<hex>` — a docker image ID for a local build, or a registry digest
// for a remote override): `dogfood-<first-12-hex>`. The sha suffix forces a
// re-pull whenever content changes, so an `IfNotPresent` node doesn't serve
// stale bits.
func dogfoodTag(contentID string) string {
	hex := strings.TrimPrefix(contentID, "sha256:")
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return "dogfood-" + hex
}

// ensureLocal makes sure a local-only dogfood `ref` is present in the host
// docker store. Such a ref names no registry (it comes from a `make images`
// build), so it can't be pulled — if it's absent, return the actionable build
// guidance. Registry-hosted refs never reach here; they go through mirrorRemote.
func ensureLocal(ctx context.Context, ref, imageName string) error {
	if inLocalDocker(ctx, ref) {
		return nil
	}
	return MissingDogfoodError([]MissingDogfood{{Name: imageName, Ref: ref}})
}

// optionCError formats the design's option-(c) failure: the message
// names the missing image, the version, and shows the exact override
// stanza the user needs to add to flywheel.yaml.local.
func optionCError(name, version, ref string, underlying error) error {
	return fmt.Errorf(`%s: could not fetch the default ghcr.io ref
  ref:   %s
  cause: %w

No published release exists for this version, and no override is set.
Build the image locally and add this to flywheel.yaml.local:

  flywheel:
    images:
      %s: <your-locally-built-tag>

For example:
  docker build -t flywheel-dev/%s:latest -f Dockerfile.%s .
  → flywheel.images.%s: flywheel-dev/%s:latest
`, name, ref, underlying, name, name, name, name, name)
}

// inLocalDocker probes the host docker store for `ref`. A package var so tests
// can stub the docker dependency.
var inLocalDocker = func(ctx context.Context, ref string) bool {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--type=image", ref)
	return cmd.Run() == nil
}

// imageContentID returns the docker image ID (`sha256:<hex>` config digest)
// of a locally-present image — a stable content address for the dogfood tag.
func imageContentID(ctx context.Context, ref string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--type=image", "--format", "{{.Id}}", ref)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", fmt.Errorf("docker inspect %s returned empty image ID", ref)
	}
	return id, nil
}

func dockerTag(ctx context.Context, src, dst string, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "tag", src, dst)
	cmd.Stdout = style.VerboseWriter(stdout)
	cmd.Stderr = style.VerboseWriter(stdout)
	return cmd.Run()
}

func dockerPush(ctx context.Context, ref string, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "push", ref)
	cmd.Stdout = style.VerboseWriter(stdout)
	cmd.Stderr = style.VerboseWriter(stdout)
	return cmd.Run()
}
