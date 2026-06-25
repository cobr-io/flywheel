// Package worktree holds the shared helpers for reasoning about an app's
// host worktree and its git provenance: where its source comes from
// (GitRemoteURL), how to materialize it (Clone), how to recognise a clone
// URL (LooksLikeGitURL), and how to read the per-app GitRepository manifests
// that record all of the above (ParseAppGitRepository / DeclaredApps).
//
// These are imported by add-app, publish-app, and up, which all need to
// agree on the same notion of an app's source. See
// docs/designs/2026-06-16-add-app-source-modes-and-local-only-guard-design.md.
package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"sigs.k8s.io/yaml"
)

// GitRemoteURL returns the `origin` remote URL of the git worktree at dir,
// and ok=false when the directory has no `origin` remote (or isn't a git
// repo / git is absent). A worktree with several remotes resolves to
// `origin` specifically — that is the canonical "where this came from".
//
// It shells out to host git deliberately (mirroring converge.CurrentBranch)
// so it inherits the developer's git configuration; see the design's
// "Clone mechanism" note on the host-git-vs-go-git choice.
func GitRemoteURL(dir string) (string, bool) {
	cmd := exec.Command("git", "-C", dir, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	url := strings.TrimSpace(string(out))
	if url == "" {
		return "", false
	}
	return url, true
}

var (
	// A clone URL with an explicit scheme.
	urlSchemeRe = regexp.MustCompile(`^(https?|ssh|git|file)://`)
	// scp-style git remote: user@host:path (no leading slash, ':' before any '/').
	scpRe = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9._-]+:`)
)

// LooksLikeGitURL reports whether arg should be treated as a clone URL rather
// than a worktree path/name. It is deliberately conservative: a bare name or a
// path (absolute or relative under workspaces_root) never matches, because
// those carry neither a URL scheme nor an scp-style `host:` prefix.
func LooksLikeGitURL(arg string) bool {
	if strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, ".") {
		return false
	}
	return urlSchemeRe.MatchString(arg) || scpRe.MatchString(arg)
}

// RepoNameFromURL extracts the repository name from a clone URL: the final
// path segment with any `.git` suffix stripped. e.g.
// https://example.com/acme/web.git → "web", git@github.com:acme/web.git → "web".
// The caller sanitises it to a worktree-dir name.
func RepoNameFromURL(url string) string {
	s := strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git")
	s = strings.TrimSuffix(s, "/") // tolerate trailing-slash-before-.git
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// App is the subset of a per-app GitRepository manifest the worktree helpers
// reason about. Source provenance no longer lives here — it is the workspace
// block in flywheel.yaml, keyed by Worktree (basename of spec.url).
type App struct {
	Name     string // metadata.name
	Worktree string // basename of spec.url, minus .git — the host worktree dir
	Path     string // path to the gitrepository.yaml
}

// ParseAppGitRepository reads a builders/base/<name>/gitrepository.yaml and
// extracts the app's name and worktree dir.
func ParseAppGitRepository(path string) (App, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return App{}, err
	}
	var doc struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			URL string `json:"url"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return App{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return App{
		Name:     doc.Metadata.Name,
		Worktree: RepoNameFromURL(doc.Spec.URL),
		Path:     path,
	}, nil
}

// DeclaredApps parses every builders/base/*/gitrepository.yaml under repoDir.
// Apps that fail to parse are skipped (best-effort discovery).
func DeclaredApps(repoDir string) ([]App, error) {
	matches, err := filepath.Glob(filepath.Join(repoDir, "builders", "base", "*", "gitrepository.yaml"))
	if err != nil {
		return nil, err
	}
	var apps []App
	for _, m := range matches {
		app, err := ParseAppGitRepository(m)
		if err != nil {
			continue
		}
		apps = append(apps, app)
	}
	return apps, nil
}

// OriginReachable reports whether `git ls-remote origin` succeeds in dir — i.e.
// the origin remote exists and is reachable with the developer's credentials.
func OriginReachable(dir string) bool {
	return exec.Command("git", "-C", dir, "ls-remote", "origin").Run() == nil
}

// BranchPushed reports whether the worktree's current-branch HEAD commit is
// already contained on origin — i.e. the developer's local commits have all
// been pushed. It fetches the remote branch tip (no working-tree changes) and
// tests whether local HEAD is an ancestor of (or equal to) that tip, so a
// remote that is *ahead* of a fully-pushed local branch still counts as pushed,
// while unpushed local commits do not. A branch absent from origin counts as
// not pushed.
func BranchPushed(dir string) (bool, error) {
	branch, err := exec.Command("git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return false, fmt.Errorf("resolve current branch: %w", err)
	}
	b := strings.TrimSpace(string(branch))
	// Fetch the remote tip into FETCH_HEAD. A failure here is almost always
	// "branch not on origin" → not pushed (origin reachability is checked
	// separately by the caller before this).
	if err := exec.Command("git", "-C", dir, "fetch", "-q", "origin", b).Run(); err != nil {
		return false, nil
	}
	// HEAD is pushed iff it is an ancestor of (or equal to) the fetched tip.
	err = exec.Command("git", "-C", dir, "merge-base", "--is-ancestor", "HEAD", "FETCH_HEAD").Run()
	if err == nil {
		return true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return false, nil // not an ancestor → unpushed commits
	}
	return false, fmt.Errorf("check ancestry against origin: %w", err)
}

// Clone clones url into dest using host git, inheriting the developer's ambient
// credentials. When branch is empty the clone lands on the remote's default
// branch (git clone's default). When branch is non-empty Clone checks it out
// after cloning and reports the result in gotBranch: a branch absent from the
// remote is NOT a clone error — Clone leaves the worktree on the remote default
// and returns gotBranch=false so the caller can warn. We clone-then-checkout
// (rather than `git clone --branch`) precisely so a missing branch degrades to
// the default instead of failing the clone and materialising no worktree.
func Clone(ctx context.Context, url, dest, branch string) (gotBranch bool, err error) {
	cmd := exec.CommandContext(ctx, "git", "clone", url, dest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return false, fmt.Errorf("git clone %s: %w\n%s", url, err, out)
	}
	if branch == "" {
		return true, nil
	}
	// `git checkout <branch>` creates a local branch tracking origin/<branch>
	// when one exists (git's DWIM), and is a no-op when the default already is
	// branch. A failure means the remote has no such branch → stay on default.
	if err := exec.CommandContext(ctx, "git", "-C", dest, "checkout", branch).Run(); err != nil {
		return false, nil
	}
	return true, nil
}
