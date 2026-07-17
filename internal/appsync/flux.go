package appsync

import (
	"context"
	"fmt"
	"time"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cobr-io/flywheel/internal/fluxpoke"
	"github.com/cobr-io/flywheel/internal/naming"
)

// gitRepoFlux is the production FluxPatcher: it patches one GitRepository via
// a controller-runtime client. Mirrors internal/selfsync.K8sFlux's shape (a
// small struct bound to one object, JSON merge patches, NotFound-is-success)
// and reproduces scripts/git-auto-sync/sync.sh's patch_gitrepository /
// trigger_reconcile annotate-then-patch semantics exactly — both idempotent,
// safe to repeat every tick. Bound to one GitRepository (name/namespace) and
// constructed once per cached Ticker (see Reconciler.tickerFor): the pair
// never changes for a given app for the Ticker's lifetime, so there is
// nothing to gain from re-allocating it every reconcile.
type gitRepoFlux struct {
	Client          client.Client
	Name, Namespace string
}

func (f *gitRepoFlux) ref() *sourcev1.GitRepository {
	return &sourcev1.GitRepository{ObjectMeta: metav1.ObjectMeta{Name: f.Name, Namespace: f.Namespace}}
}

// EnsureBranch stops kustomize-controller from re-reconciling this
// GitRepository — so a periodic re-apply of the source manifest's static
// `branch: main` can't race the patch below — then patches spec.ref.branch.
// Both are JSON merge patches (touch only the one key, never conflict with
// Flux's own status writes) applied in the same order sync.sh's
// patch_gitrepository used: annotate first, patch second. A NotFound on
// either (the GR was deleted mid-tick) is not an error — nothing to patch,
// and the next tick's fresh Get in Reconcile sorts out whether the app is
// still live.
func (f *gitRepoFlux) EnsureBranch(ctx context.Context, branch string) error {
	annotate := fmt.Appendf(nil, `{"metadata":{"annotations":{%q:%q}}}`,
		naming.KustomizeReconcileDisabledAnnotation, naming.KustomizeReconcileDisabledValue)
	if err := f.patch(ctx, annotate); err != nil {
		return fmt.Errorf("annotate %s/%s reconcile-disabled: %w", f.Namespace, f.Name, err)
	}
	spec := fmt.Appendf(nil, `{"spec":{"ref":{"branch":%q}}}`, branch)
	if err := f.patch(ctx, spec); err != nil {
		return fmt.Errorf("patch %s/%s spec.ref.branch=%s: %w", f.Namespace, f.Name, branch, err)
	}
	return nil
}

// PokeReconcile bumps naming.ReconcileRequestAnnotation via the shared
// internal/fluxpoke implementation — the same poke internal/selfsync.K8sFlux
// uses for the gitops/self path.
func (f *gitRepoFlux) PokeReconcile(ctx context.Context) error {
	return fluxpoke.Poke(ctx, f.Client, f.ref(), time.Now())
}

// patch applies a JSON merge patch to the bound GitRepository, treating a
// missing object as success (mirrors internal/selfsync.K8sFlux.patch).
func (f *gitRepoFlux) patch(ctx context.Context, body []byte) error {
	err := f.Client.Patch(ctx, f.ref(), client.RawPatch(types.MergePatchType, body))
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}
