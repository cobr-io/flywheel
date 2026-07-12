package main

import (
	"testing"

	sourcev1 "github.com/fluxcd/source-controller/api/v1"
)

// The deploy loop's typed GitRepository access (selfsync.K8sFlux) fails on
// every call if sourcev1 is missing from the client scheme — a runtime-only
// failure no fake-client test catches, because those register the scheme
// themselves. This pins the real constructor.
func TestNewScheme_RegistersSourceV1(t *testing.T) {
	s, err := newScheme()
	if err != nil {
		t.Fatalf("newScheme: %v", err)
	}
	gvk := sourcev1.GroupVersion.WithKind(sourcev1.GitRepositoryKind)
	if !s.Recognizes(gvk) {
		t.Fatalf("scheme does not recognize %s — typed K8sFlux calls would fail at runtime", gvk)
	}
}
