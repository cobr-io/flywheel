package converge

import (
	"fmt"
	"io/fs"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"github.com/cobr-io/flywheel/internal/cli/applier"
)

// devLoopOverlay is the dev-loop tree both `up`'s direct apply and the Flux
// flywheel-dev-loop Kustomization render (see ApplyDevLoop).
const devLoopOverlay = "manifests/dev-loop/overlays/local"

// DeclaredDevLoopPVCs returns the PersistentVolumeClaims the dev-loop
// manifests in assets declare (buildkit-cache, git-server-data, ...). These
// are live state, not orphans: `flywheel clean` keeps them (issue #172).
// assets is the binary's embedded tree (flywheel.Assets) — the same version
// `up` applied, since `up` refuses to run on version drift. Built in memory,
// so no cache dir or cluster is needed.
func DeclaredDevLoopPVCs(assets fs.FS) ([]applier.ResourceRef, error) {
	mem := filesys.MakeFsInMemory()
	err := fs.WalkDir(assets, "manifests", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return mem.MkdirAll("/" + p)
		}
		raw, err := fs.ReadFile(assets, p)
		if err != nil {
			return err
		}
		return mem.WriteFile("/"+p, raw)
	})
	if err != nil {
		return nil, fmt.Errorf("read embedded manifests: %w", err)
	}
	rm, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(mem, "/"+devLoopOverlay)
	if err != nil {
		return nil, fmt.Errorf("build %s: %w", devLoopOverlay, err)
	}
	var pvcs []applier.ResourceRef
	for _, r := range rm.Resources() {
		if r.GetKind() == "PersistentVolumeClaim" {
			pvcs = append(pvcs, applier.ResourceRef{Kind: r.GetKind(), Namespace: r.GetNamespace(), Name: r.GetName()})
		}
	}
	return pvcs, nil
}
