// Package mirror pushes the cached Flywheel clone into the in-cluster
// git-server as a second bare repo `flywheel.git`. This is `flywheel up`
// step 11c — the move that makes the dev loop truly offline after first
// bootstrap (per design § Architecture: "neither GitRepository points
// at GitHub at runtime").
//
// Implementation: client-go port-forward to the git-server Service +
// go-git push over HTTP to localhost:<forwarded>. No host `git`
// shell-out (per design § Prerequisites).
package mirror

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-git/go-git/v5"
	gitconfig "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/cobr-io/flywheel/internal/cli/applier"
	"github.com/cobr-io/flywheel/internal/cli/style"
)

// Push uploads `cacheDir` (a checked-out clone) to the in-cluster
// git-server as bare repo `repoName`.git, tagged at `wantSHA`.
//
// Idempotent: if the in-cluster remote already has `wantSHA` as a
// reachable commit, returns nil without re-pushing.
func Push(ctx context.Context, kubeconfigPath, contextName, namespace, svcName, repoName, cacheDir, wantSHA string, out io.Writer) error {
	if err := applier.WaitForServiceReady(ctx, kubeconfigPath, contextName, namespace, svcName, 60*time.Second); err != nil {
		return err
	}
	pf, err := applier.ForwardToService(ctx, kubeconfigPath, contextName, namespace, svcName, 8080, out)
	if err != nil {
		return err
	}
	defer pf.Close()

	remoteURL := fmt.Sprintf("http://localhost:%d/%s.git", pf.LocalPort, repoName)

	// Already at the expected SHA? Check via ls-remote. Caller wraps
	// this whole function in style.Spin, which owns `out` for the
	// duration; surface our own progress only in verbose mode so the
	// Spin redraw stays uncorrupted in normal use.
	if has, err := remoteHasSHA(remoteURL, wantSHA); err == nil && has {
		style.OKv(out, "in-cluster %s.git already at %s", repoName, wantSHA[:12])
		return nil
	}

	repo, err := git.PlainOpen(cacheDir)
	if err != nil {
		return fmt.Errorf("open cache %s: %w", cacheDir, err)
	}

	// Add or update the in-cluster remote.
	const remoteName = "incluster"
	_ = repo.DeleteRemote(remoteName)
	_, err = repo.CreateRemote(&gitconfig.RemoteConfig{
		Name: remoteName,
		URLs: []string{remoteURL},
	})
	if err != nil {
		return fmt.Errorf("create remote %s: %w", remoteURL, err)
	}

	// Push refs/heads/main → refs/heads/main. The cache (extracted +
	// committed by embedcache.Populate) initialises HEAD on `main`; we
	// resolve the explicit ref rather than `HEAD` because go-git's
	// PushContext silently no-ops the `HEAD:refs/heads/main` source-side
	// shorthand against an empty remote repo. Flux's flywheel-source
	// pins by `spec.ref.commit`, which only needs the commit reachable
	// from some ref. The git-server entrypoint pre-creates an empty
	// flywheel.git bare repo (with receive-pack enabled) so this first
	// push has a target.
	err = repo.PushContext(ctx, &git.PushOptions{
		RemoteName: remoteName,
		RefSpecs:   []gitconfig.RefSpec{"+refs/heads/main:refs/heads/main"},
		Force:      true,
		Progress:   style.VerboseWriter(out), // git-push progress chatter
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("push to %s: %w", remoteURL, err)
	}
	style.OKv(out, "pushed cache → in-cluster %s.git (SHA %s)", repoName, wantSHA[:12])
	return nil
}

func remoteHasSHA(remoteURL, sha string) (bool, error) {
	rem := git.NewRemote(nil, &gitconfig.RemoteConfig{
		Name: "probe",
		URLs: []string{remoteURL},
	})
	refs, err := rem.List(&git.ListOptions{})
	if err != nil {
		// "not found" / 404 means the bare repo doesn't exist yet —
		// caller should push to create it.
		return false, err
	}
	want := plumbing.NewHash(sha)
	for _, r := range refs {
		if r.Hash() == want {
			return true, nil
		}
	}
	return false, nil
}

// HTTPClient is exported so callers can plug in a custom transport for
// tests (e.g. inject mock 404 / timeout behaviour). Unused at present.
var HTTPClient = &http.Client{Timeout: 30 * time.Second}
