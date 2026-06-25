package controller

import (
	"strings"
	"testing"
)

// Invariant: no client-specific literals reachable in the binary; every
// client-specific value flows through Config. This test exercises the
// parameterisation surface.

func TestConfigValidate_AllRequiredFields(t *testing.T) {
	full := Config{
		Namespace:        "flywheel-system",
		BuilderNamespace: "flywheel-system",
		Registry:         "acme-local-registry",
		RegistryPort:     "50001",
		ClusterName:      "acme-local",
		ClientName:       "acme",
	}
	if err := full.Validate(); err != nil {
		t.Fatalf("Validate on full Config returned %v, want nil", err)
	}
}

func TestConfigValidate_MissingFieldsReported(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*Config)
		expected string
	}{
		{"missing namespace", func(c *Config) { c.Namespace = "" }, "FLYWHEEL_NAMESPACE"},
		{"missing builder namespace", func(c *Config) { c.BuilderNamespace = "" }, "BUILDER_NAMESPACE"},
		{"missing registry", func(c *Config) { c.Registry = "" }, "CLUSTER_REGISTRY"},
		{"missing registry port", func(c *Config) { c.RegistryPort = "" }, "CLUSTER_REGISTRY_PORT"},
		{"missing cluster name", func(c *Config) { c.ClusterName = "" }, "CLUSTER_NAME"},
		{"missing client name", func(c *Config) { c.ClientName = "" }, "CLIENT_NAME"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Config{
				Namespace:        "flywheel-system",
				BuilderNamespace: "flywheel-system",
				Registry:         "acme-local-registry",
				RegistryPort:     "50001",
				ClusterName:      "acme-local",
				ClientName:       "acme",
			}
			tc.mutate(&c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate with %s succeeded, want error", tc.name)
			}
			if !strings.Contains(err.Error(), tc.expected) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.expected)
			}
		})
	}
}

func TestConfigValidate_AllMissing(t *testing.T) {
	err := Config{}.Validate()
	if err == nil {
		t.Fatal("empty Config validated, want error listing all six fields")
	}
	for _, want := range []string{
		"FLYWHEEL_NAMESPACE", "BUILDER_NAMESPACE", "CLUSTER_REGISTRY",
		"CLUSTER_REGISTRY_PORT", "CLUSTER_NAME", "CLIENT_NAME",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing field %q", err.Error(), want)
		}
	}
}

func TestRegistryURL_UsesInClusterPortNotHostPort(t *testing.T) {
	// In-cluster registry references always use the container port (5000),
	// not the host-side cluster.registry_port (e.g. 50001).
	cases := []struct {
		registry string
		hostPort string
		want     string
	}{
		{"acme-local-registry", "50001", "k3d-acme-local-registry:5000"},
		{"dogfood-registry", "50042", "k3d-dogfood-registry:5000"},
	}
	for _, tc := range cases {
		c := Config{Registry: tc.registry, RegistryPort: tc.hostPort}
		if got := c.RegistryURL(); got != tc.want {
			t.Errorf("RegistryURL(%q, hostPort=%q) = %q, want %q", tc.registry, tc.hostPort, got, tc.want)
		}
	}
}
