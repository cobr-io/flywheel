package initcmd

import (
	"io/fs"
	"os"
	"testing"
)

// TestEmbeddedSkeletonMirrorsDisk guards the production render path:
// `flywheel init` renders embeddedSkeleton() (a fs.Sub of the binary's
// embedded flywheel.Assets), NOT the on-disk tree the golden tests use
// via os.DirFS (see skeletonDir/runInitForGolden in init_test.go).
// Because the golden path bypasses embed, it cannot catch a
// `//go:embed` pattern silently excluding a file from the binary — that
// once dropped templates/client-skeleton/.gitignore and .sops.yaml from
// generated repos.
//
// Rather than hardcode the two files that bug happened to hit (which
// would stay silent for a *third* omitted file), this walks every file
// under templates/client-skeleton on disk and asserts embeddedSkeleton()
// — the actual fs.Sub production code calls — has each one.
func TestEmbeddedSkeletonMirrorsDisk(t *testing.T) {
	skel := embeddedSkeleton()
	dir := skeletonDir(t)

	err := fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if _, err := fs.Stat(skel, p); err != nil {
			t.Errorf("embedded skeleton sub-FS missing %q (flywheel init would not emit it; check embed.go's `all:` prefix): %v", p, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}
