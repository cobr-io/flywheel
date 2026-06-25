package embedcache

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestAssetContentSHA_DeterministicAndContentSensitive(t *testing.T) {
	base := fstest.MapFS{
		"root/a.txt":     {Data: []byte("alpha")},
		"root/sub/b.txt": {Data: []byte("beta")},
	}
	sha1, err := AssetContentSHA(base, "root")
	if err != nil {
		t.Fatal(err)
	}
	// Stable across calls.
	sha2, err := AssetContentSHA(base, "root")
	if err != nil {
		t.Fatal(err)
	}
	if sha1 != sha2 {
		t.Errorf("non-deterministic: %q != %q", sha1, sha2)
	}

	// A content change flips the hash (so the image tag changes → roll).
	changed := fstest.MapFS{
		"root/a.txt":     {Data: []byte("ALPHA")},
		"root/sub/b.txt": {Data: []byte("beta")},
	}
	shaChanged, err := AssetContentSHA(changed, "root")
	if err != nil {
		t.Fatal(err)
	}
	if shaChanged == sha1 {
		t.Error("content change did not alter the hash")
	}
}

// TestPopulate_RebustsOnContentChangeSameVersion is the stale-cache trap
// regression: a dev binary keeps version "v0.0.0-dev" across rebuilds, so a
// content change at the same version must still re-extract (not serve the
// first extraction forever).
func TestPopulate_RebustsOnContentChangeSameVersion(t *testing.T) {
	root := t.TempDir()
	const version = "v0.0.0-dev"

	_, sha1, err := Populate(root, version, fstest.MapFS{
		"assets/a.txt": {Data: []byte("one")},
	}, "assets")
	if err != nil {
		t.Fatal(err)
	}

	// Same version, changed content.
	dest, sha2, err := Populate(root, version, fstest.MapFS{
		"assets/a.txt": {Data: []byte("two")},
	}, "assets")
	if err != nil {
		t.Fatal(err)
	}
	if sha1 == sha2 {
		t.Error("stale cache: content change at same version did not re-extract")
	}

	// The re-extracted dir holds the new content.
	got, err := readFile(t, dest, "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != "two" {
		t.Errorf("re-extracted content = %q, want %q", got, "two")
	}

	// Idempotent: same content again is a cache hit (stable SHA).
	_, sha3, err := Populate(root, version, fstest.MapFS{
		"assets/a.txt": {Data: []byte("two")},
	}, "assets")
	if err != nil {
		t.Fatal(err)
	}
	if sha3 != sha2 {
		t.Errorf("unchanged content re-extracted: %q != %q", sha3, sha2)
	}
}

func readFile(t *testing.T, dir, rel string) (string, error) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	return string(b), err
}

// TestAssetContentSHA_PathBoundary guards against the classic concat
// collision: "a"+"bc" vs "ab"+"c" must not hash equal (the length prefix
// prevents it).
func TestAssetContentSHA_PathBoundary(t *testing.T) {
	x, err := AssetContentSHA(fstest.MapFS{
		"r/ab": {Data: []byte("c")},
	}, "r")
	if err != nil {
		t.Fatal(err)
	}
	y, err := AssetContentSHA(fstest.MapFS{
		"r/a": {Data: []byte("bc")},
	}, "r")
	if err != nil {
		t.Fatal(err)
	}
	if x == y {
		t.Error("path/content boundary collision")
	}
}
