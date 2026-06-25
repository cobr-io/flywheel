package controller

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
func canList(cfg *rest.Config, gvk schema.GroupVersionKind) (bool, error) {
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
	if err := c.List(context.Background(), list, client.Limit(1)); err != nil {
		return false, err
	}
	return true, nil
}
