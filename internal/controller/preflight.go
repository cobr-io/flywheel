package controller

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// preflightRetryInterval is how often a gated poke controller re-probes for the
// resource kind it needs after an initial denial. Slow on purpose: the RBAC/CRD
// almost always lands within seconds of startup, and once it does the controller
// is registered for the pod's lifetime — this ticker only has to cover the
// transient startup race, so a tight interval would just burn API calls idling.
const preflightRetryInterval = 5 * time.Minute

// canList probes whether the controller's ServiceAccount can list the given
// resource kind, using a direct (uncached) client built from cfg. It returns
// false on an RBAC denial or a missing CRD.
//
// The optional poke reconcilers call this before registering their watch.
// Registering a watch the controller cannot list makes the manager's cache
// sync time out at startup, which exits the manager and CRASH-LOOPS the whole
// pod — taking the core build path (GitRepository -> build Job) down with it.
// That is exactly what happens when Flux re-asserts the mirror RBAC (without
// the poke rules) under a controller image that still wires the pokes. Gating
// on canList degrades that to "this one poke is disabled" and the build loop
// keeps running; the poke simply falls back to its Flux poll interval.
func canList(ctx context.Context, cfg *rest.Config, gvk schema.GroupVersionKind) (bool, error) {
	c, err := client.New(cfg, client.Options{})
	if err != nil {
		return false, err
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   gvk.Group,
		Version: gvk.Version,
		Kind:    gvk.Kind + "List",
	})
	if err := c.List(ctx, list, client.Limit(1)); err != nil {
		return false, err
	}
	return true, nil
}

// setupWhenListable registers a poke controller now if its required resource
// kind is listable, or — when it is not yet (RBAC not landed, CRD absent) —
// defers registration to a slow-ticker re-probe so the poke recovers on its own
// once the permission appears, instead of staying disabled until a pod restart.
// register is the controller's own builder wiring (For(...).Complete(r)); it is
// invoked at most once, either inline here or later from the re-probe runnable.
//
// This keeps the fix minimal: the manager is never rebuilt. The gate rides on
// the manager's own runnable machinery (mgr.Add works before and after Start),
// and a controller registered after Start is enqueued and started immediately.
func setupWhenListable(mgr ctrl.Manager, name string, gvk schema.GroupVersionKind, register func() error) error {
	log := mgr.GetLogger().WithName(name)
	if ok, _ := canList(context.Background(), mgr.GetConfig(), gvk); ok {
		return register()
	}
	return mgr.Add(newListGate(log, name, gvk,
		func(ctx context.Context) (bool, error) { return canList(ctx, mgr.GetConfig(), gvk) },
		register))
}

// listGatedRunnable re-probes for a poke controller's required resource kind on
// a slow ticker and registers the controller the first time the probe succeeds,
// then stops. It is what makes an initial preflight denial *recoverable* without
// a pod restart. It implements manager.Runnable.
type listGatedRunnable struct {
	name     string
	gvk      schema.GroupVersionKind
	log      logr.Logger
	interval time.Duration
	probe    func(context.Context) (bool, error) // true once the kind is listable
	register func() error                        // controller builder wiring; invoked at most once
}

// newListGate constructs the runnable and emits the initial degraded-mode log.
// logr has no Warn level, so the higher-visibility signal the plan asks for
// (louder than the previous Info) is an Error carrying the probe's cause; the
// message states plainly that this is transient and self-healing.
func newListGate(log logr.Logger, name string, gvk schema.GroupVersionKind, probe func(context.Context) (bool, error), register func() error) *listGatedRunnable {
	ok, err := probe(context.Background())
	if !ok {
		log.Error(err, "poke disabled: required Flux resource kind is not listable yet"+
			" (RBAC not landed or CRD absent); re-probing on an interval and enabling"+
			" without a pod restart once the permission appears — Flux poll covers the gap",
			"kind", gvk.Kind, "retryInterval", preflightRetryInterval.String())
	}
	return &listGatedRunnable{
		name:     name,
		gvk:      gvk,
		log:      log,
		interval: preflightRetryInterval,
		probe:    probe,
		register: register,
	}
}

func (g *listGatedRunnable) Start(ctx context.Context) error {
	t := time.NewTicker(g.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			ok, err := g.probe(ctx)
			if !ok {
				g.log.V(1).Info("poke still disabled; will retry", "kind", g.gvk.Kind, "error", err)
				continue
			}
			if err := g.register(); err != nil {
				// Registering after the manager has started is expected to
				// succeed (the runnable machinery enqueues and starts it); a
				// failure here is unusual, so log and keep retrying rather than
				// silently give up.
				g.log.Error(err, "re-probe succeeded but registering the poke controller failed; will retry", "kind", g.gvk.Kind)
				continue
			}
			g.log.Info("poke enabled after re-probe: resource kind is now listable", "kind", g.gvk.Kind)
			return nil
		}
	}
}
