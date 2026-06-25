package initcmd

import (
	"io/fs"
	"testing"
)

// TestEmbeddedSkeletonHasDotfiles guards the production render path:
// `flywheel init` renders embeddedSkeleton() (a fs.Sub of the binary's
// embedded Assets), NOT the on-disk tree the golden tests use via
// os.DirFS. Because the golden path bypasses embed, it could not catch
// the //go:embed dotfile exclusion that once dropped .gitignore and
// .sops.yaml from generated repos. This test exercises the real sub-FS.
func TestEmbeddedSkeletonHasDotfiles(t *testing.T) {
	skel := embeddedSkeleton()
	for _, name := range []string{".gitignore.tmpl", ".sops.yaml.tmpl"} {
		if _, err := fs.Stat(skel, name); err != nil {
			t.Errorf("embedded skeleton sub-FS missing %q (flywheel init would not emit it): %v", name, err)
		}
	}
}

// TestEmbeddedGuidesShipsGuide guards the production guide-copy path:
// `flywheel init` copies embeddedGuides() (a fs.Sub of the binary's embedded
// Assets at docs/guides), NOT the on-disk dir the golden tests inject via
// os.DirFS. So a missing //go:embed docs/guides directive would pass the
// goldens but ship no guide to real users. This test exercises the real
// sub-FS.
func TestEmbeddedGuidesShipsGuide(t *testing.T) {
	guides := embeddedGuides()
	if _, err := fs.Stat(guides, "onboarding.md"); err != nil {
		t.Errorf("embedded guides sub-FS missing onboarding.md (flywheel init would not emit it): %v", err)
	}
}
