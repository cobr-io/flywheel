package main

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestManagerOptionsBypassSecretCache(t *testing.T) {
	opts := managerOptions(":8081")
	if opts.Client.Cache == nil {
		t.Fatal("manager client cache options are nil")
	}

	for _, obj := range opts.Client.Cache.DisableFor {
		if _, ok := obj.(*corev1.Secret); ok {
			return
		}
	}

	t.Fatal("Secrets must bypass the informer cache to preserve namespaced get-only RBAC")
}
