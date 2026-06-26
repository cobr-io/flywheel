// Package imagepin resolves the three Flywheel container image
// references (git-server, git-auto-sync, image-builder-controller)
// from a client's config and makes sure each is reachable by every node
// in the k3d cluster at `flywheel up` time.
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
//   - Default (released) ref `ghcr.io/cobr-io/<name>:<version>`: ensure
//     the image is in the host docker store (`docker pull` if absent),
//     then tag+push it into the local registry under the immutable
//     `:<version>` tag (the version IS the content address for a release).
//     If the pull fails and no override is set, return option (c) — a
//     clear error naming the override stanza for `flywheel.yaml.local`.
//
//   - Override (dogfood) ref: ensure the image is in the host docker store
//     (`docker pull` if it's a remote ref), then tag+push it into the
//     local registry under a content-addressed `:dogfood-<sha>` tag. The
//     sha suffix forces a re-pull on change, so the bare `IfNotPresent`
//     pull policy never serves a stale image off a mutable `:dogfood` tag.
package imagepin

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/cli/style"
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

// Resolve returns the map of image name → resolved ref for the three known
// images, honouring `cfg.Flywheel.Images` overrides. A bare `:dogfood`
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
	Name string // image name: git-server, git-auto-sync, image-builder-controller
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

// EnsureInRegistry is the `flywheel add app` counterpart of EnsureInCluster.
// add-app runs after `up`, which has already mirrored all three images into
// the local registry, so this only computes the in-cluster pull ref the
// per-app git-auto-sync Deployment references. A default is already in the
// registry from `up` (ref computed, no docker work — add-app stays thin); an
// override is (re-)pushed to be safe.
func EnsureInRegistry(ctx context.Context, ref, registryName string, registryPort int, imageName, version string, stdout io.Writer) (string, error) {
	if IsDefault(imageName, version, ref) {
		_, pull := registryRefs(registryName, registryPort, imageName, version)
		return pull, nil
	}
	return mirrorToRegistry(ctx, ref, registryName, registryPort, imageName, version, stdout)
}

// mirrorToRegistry ensures `ref` is in the host docker store (pull if it's a
// remote/default ref), then tags+pushes it into the cluster's local registry
// and returns the in-cluster pull ref. The tag is the immutable `:<version>`
// for a released ref, or content-addressed `:dogfood-<sha>` for an override.
func mirrorToRegistry(ctx context.Context, ref, registryName string, registryPort int, imageName, version string, stdout io.Writer) (string, error) {
	if err := ensureLocal(ctx, ref, imageName, version, stdout); err != nil {
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

// dogfoodTag derives the content-addressed tag from a docker image ID
// (`sha256:<hex>`): `dogfood-<first-12-hex>`. The sha suffix forces a
// re-pull whenever content changes, so an `IfNotPresent` node doesn't serve
// stale bits.
func dogfoodTag(contentID string) string {
	hex := strings.TrimPrefix(contentID, "sha256:")
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return "dogfood-" + hex
}

// ensureLocal makes sure `ref` is present in the host docker store, pulling
// it if absent. A missing local-only dogfood override names no registry, so a
// pull is doomed — return the build guidance instead of attempting it. On a
// pull failure for the unmodified default ghcr.io ref it returns the option-(c)
// override guidance.
func ensureLocal(ctx context.Context, ref, imageName, version string, stdout io.Writer) error {
	if inLocalDocker(ctx, ref) {
		return nil
	}
	if isLocalOnlyOverride(imageName, version, ref) {
		return MissingDogfoodError([]MissingDogfood{{Name: imageName, Ref: ref}})
	}
	if err := dockerPull(ctx, ref, stdout); err != nil {
		if IsDefault(imageName, version, ref) {
			return optionCError(imageName, version, ref, err)
		}
		return fmt.Errorf("pull %s: %w", ref, err)
	}
	return nil
}

// optionCError formats the design's option-(c) failure: the message
// names the missing image, the version, and shows the exact override
// stanza the user needs to add to flywheel.yaml.local.
func optionCError(name, version, ref string, underlying error) error {
	return fmt.Errorf(`%s: pull failed for the default ghcr.io ref
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

func dockerPull(ctx context.Context, ref string, stdout io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", "pull", ref)
	cmd.Stdout = style.VerboseWriter(stdout)
	cmd.Stderr = style.VerboseWriter(stdout)
	return cmd.Run()
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
