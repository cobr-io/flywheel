// Package embedcache extracts the binary's embedded asset tree
// (`flywheel.Assets`) to a stable on-disk cache directory and commits
// it to a real git repo there, so `flywheel up` step 11c can push it
// into the in-cluster Flywheel mirror just like the pre-embed cache.
//
// This replaces the gitcache.EnsureClone path: no network, no
// FLYWHEEL_REPO_URL, no git tag — the cache content is whatever the
// running binary embeds.
//
// The commit is deterministic (fixed author + email + message + epoch
// timestamp), so the resulting SHA is stable across runs of the same
// binary and the client's flywheel-source.yaml doesn't churn between
// `up` invocations.
package embedcache

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// AssetContentSHA returns a deterministic, version-independent hash of the
// embedded asset tree under `root` in `fsys`: sha256 over every file's
// relative path and bytes, in sorted path order. Used as the image content
// tag (`dogfood-<sha[:12]>`) so a Deployment rolls iff the image content
// changes — unlike EnsureCommitted's commit SHA, which embeds the version
// string and so can't be reproduced by a repo-agnostic `make images`.
func AssetContentSHA(fsys fs.FS, root string) (string, error) {
	var paths []string
	err := fs.WalkDir(fsys, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		rel := strings.TrimPrefix(strings.TrimPrefix(p, root), "/")
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return "", err
		}
		// Length-prefix path + bytes so no two trees collide via
		// boundary ambiguity.
		fmt.Fprintf(h, "%s\x00%d\x00", rel, len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DefaultRoot is ~/.cache/flywheel/, expanded for the running user.
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cache", "flywheel"), nil
}

// Extract writes every file under `srcRoot` in `fsys` to `dest` on disk,
// preserving the tree structure. Returns nil if dest already matches
// (idempotent — caller can call every `up`).
func Extract(fsys fs.FS, srcRoot, dest string) error {
	return fs.WalkDir(fsys, srcRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, srcRoot)
		rel = strings.TrimPrefix(rel, "/")
		if rel == "" {
			return os.MkdirAll(dest, 0o755)
		}
		out := filepath.Join(dest, filepath.FromSlash(rel))
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		in, err := fsys.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		outF, err := os.Create(out)
		if err != nil {
			return err
		}
		if _, err := io.Copy(outF, in); err != nil {
			outF.Close()
			return err
		}
		return outF.Close()
	})
}

// EnsureCommitted ensures `dest` is a git repo with a single
// deterministic commit containing every file currently in the
// directory. Returns the commit SHA. Idempotent: if a matching commit
// already exists, re-running is a no-op.
//
// Determinism: fixed author/committer (flywheel/embedded@flywheel),
// fixed message ("flywheel embedded assets <version>"), fixed time
// (unix epoch). Same input → same SHA.
func EnsureCommitted(dest, version string) (string, error) {
	// Idempotency check: if dest is already a git repo with a HEAD
	// commit, return its SHA. Caller is responsible for cache busting
	// (e.g. clean the dir) when the binary's content changed.
	if repo, err := git.PlainOpen(dest); err == nil {
		if head, err := repo.Head(); err == nil {
			return head.Hash().String(), nil
		}
	}
	// Fresh init. Set the initial branch to `main` so mirror.Push's
	// `+HEAD:refs/heads/main` refspec finds a non-empty source — go-git
	// PlainInit otherwise defaults to `master`, in which case HEAD →
	// refs/heads/master and the cross-branch push silently no-ops.
	repo, err := git.PlainInitWithOptions(dest, &git.PlainInitOptions{
		InitOptions: git.InitOptions{
			DefaultBranch: plumbing.Main,
		},
		Bare: false,
	})
	if err != nil {
		return "", fmt.Errorf("git init %s: %w", dest, err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		return "", err
	}
	if err := wt.AddGlob("."); err != nil {
		return "", fmt.Errorf("git add: %w", err)
	}
	t := time.Unix(0, 0).UTC()
	hash, err := wt.Commit(fmt.Sprintf("flywheel embedded assets %s", version), &git.CommitOptions{
		Author:            &object.Signature{Name: "flywheel", Email: "embedded@flywheel", When: t},
		Committer:         &object.Signature{Name: "flywheel", Email: "embedded@flywheel", When: t},
		AllowEmptyCommits: false,
	})
	if err != nil {
		return "", fmt.Errorf("git commit: %w", err)
	}
	return hash.String(), nil
}

// Populate runs Extract + EnsureCommitted in one shot against the
// canonical cache layout (`<root>/<version>/`). Returns the cache
// directory + commit SHA.
func Populate(root, version string, fsys fs.FS, prefix string) (string, string, error) {
	if root == "" || version == "" {
		return "", "", errors.New("embedcache.Populate: root and version required")
	}
	dest := filepath.Join(root, version)
	// Cache identity = version + content hash. The content hash is what
	// busts a stale cache: a dev binary keeps version "v0.0.0-dev" across
	// rebuilds, so keying on version alone would serve the first
	// extraction forever even after the embedded manifests change. Hashing
	// the embed tree makes any content change re-extract.
	contentHash, err := AssetContentSHA(fsys, prefix)
	if err != nil {
		return "", "", fmt.Errorf("asset content sha: %w", err)
	}
	identity := version + "|" + contentHash
	// For idempotency, an existing valid git repo whose marker matches the
	// current identity is a cache hit — re-read its SHA.
	if existing, err := readMarker(dest); err == nil && existing == identity {
		repo, err := git.PlainOpen(dest)
		if err == nil {
			if head, err := repo.Head(); err == nil {
				return dest, head.Hash().String(), nil
			}
		}
	}
	// Cache miss or stale: wipe + extract.
	if err := os.RemoveAll(dest); err != nil {
		return "", "", err
	}
	if err := Extract(fsys, prefix, dest); err != nil {
		return "", "", fmt.Errorf("extract embed: %w", err)
	}
	if err := writeMarker(dest, identity); err != nil {
		return "", "", err
	}
	sha, err := EnsureCommitted(dest, version)
	if err != nil {
		return "", "", err
	}
	return dest, sha, nil
}

const markerFile = ".flywheel-embed-version"

func readMarker(dest string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dest, markerFile))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// writeMarker records the cache identity (version + content hash). See
// Populate for why the content hash is the busting key.
func writeMarker(dest, identity string) error {
	return os.WriteFile(filepath.Join(dest, markerFile), []byte(identity+"\n"), 0o644)
}
