package flywheel

import (
	"io/fs"
	"os"
	"testing"
)

// TestEmbedMirrorsDisk guards against the `//go:embed` dotfile gotcha:
// patterns without the `all:` prefix silently exclude files whose names
// begin with "." or "_". That once dropped templates/client-skeleton/
// .gitignore and .sops.yaml from the binary, so `flywheel init` emitted
// neither. This test walks the on-disk source tree and asserts every
// file is also present in the embedded Assets FS — so any file the embed
// directive silently omits (dotfile or otherwise) fails the build's tests.
func TestEmbedMirrorsDisk(t *testing.T) {
	for _, root := range []string{"templates", "manifests"} {
		t.Run(root, func(t *testing.T) {
			err := fs.WalkDir(os.DirFS("."), root, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				if _, err := fs.Stat(Assets, p); err != nil {
					t.Errorf("on-disk file %q is missing from embedded Assets (likely a //go:embed dotfile exclusion — use the all: prefix): %v", p, err)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("walk %s: %v", root, err)
			}
		})
	}
}
