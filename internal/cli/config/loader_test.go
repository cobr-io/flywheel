package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cobr-io/flywheel/internal/cli/schema"
	"github.com/cobr-io/flywheel/internal/naming"
)

// validYAML is a fully-populated committed flywheel.yaml that passes
// schema.Validate — the baseline the ValidateFull cases build on.
const validYAML = `schema: v1alpha1
flywheel:
  version: v0.1.0
client:
  name: acme
cluster:
  name: acme-local
  registry: acme-registry
  registry_port: 50001
  http_port: 8080
  https_port: 8540
namespaces:
  flywheel: flywheel-system
  apps: apps
flux:
  interval_local: 10s
`

// minimalYAML parses (strict) but omits namespaces + flux entirely, so the
// loader's defaults must fill them. It is NOT valid under schema.Validate.
const minimalYAML = `schema: v1alpha1
flywheel:
  version: v0.1.0
client:
  name: acme
cluster:
  name: acme-local
  registry: acme-registry
  registry_port: 50001
  http_port: 8080
  https_port: 8540
`

// writeRepo writes a committed flywheel.yaml (and, when non-empty, a
// flywheel.yaml.local) into a fresh temp dir and returns it.
func writeRepo(t *testing.T, committed, local string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, naming.ConfigFile), []byte(committed), 0o644); err != nil {
		t.Fatal(err)
	}
	if local != "" {
		if err := os.WriteFile(filepath.Join(dir, naming.ConfigFileLocal), []byte(local), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoad(t *testing.T) {
	// A committed file that is otherwise valid but carries a .local-only
	// field: schema.Validate (Full) rejects it, but None/LocalOnly do not.
	committedWithPaths := validYAML + `paths:
  workspaces_root: /abs/from/committed
`
	tests := []struct {
		name      string
		committed string
		local     string
		opts      LoadOptions
		wantErr   string // substring; "" = expect success
		check     func(*testing.T, *schema.File)
	}{
		{
			name:      "none parses valid file, defaults are no-ops",
			committed: validYAML,
			opts:      LoadOptions{Validate: ValidateNone},
			check: func(t *testing.T, f *schema.File) {
				if f.Cluster.Name != "acme-local" || f.Namespaces.Apps != "apps" || f.Flux.IntervalLocal != "10s" {
					t.Errorf("unexpected values: %+v", f)
				}
			},
		},
		{
			name:      "none fills namespace + flux defaults when unset",
			committed: minimalYAML,
			opts:      LoadOptions{Validate: ValidateNone},
			check: func(t *testing.T, f *schema.File) {
				if f.Namespaces.Apps != "apps" {
					t.Errorf("namespaces.apps default = %q, want apps", f.Namespaces.Apps)
				}
				if f.Namespaces.Flywheel != naming.FlywheelNamespace {
					t.Errorf("namespaces.flywheel default = %q, want %q", f.Namespaces.Flywheel, naming.FlywheelNamespace)
				}
				if f.Flux.IntervalLocal != "10s" {
					t.Errorf("flux.interval_local default = %q, want 10s", f.Flux.IntervalLocal)
				}
			},
		},
		{
			name:      "none does not validate committed .local-only field",
			committed: committedWithPaths,
			opts:      LoadOptions{Validate: ValidateNone},
			check: func(t *testing.T, f *schema.File) {
				if f.Paths.WorkspacesRoot != "/abs/from/committed" {
					t.Errorf("workspaces_root = %q", f.Paths.WorkspacesRoot)
				}
			},
		},
		{
			name:      "full rejects committed carrying paths.workspaces_root",
			committed: committedWithPaths,
			opts:      LoadOptions{Validate: ValidateFull},
			wantErr:   "validate " + naming.ConfigFile,
		},
		{
			name:      "full accepts valid committed + absolute .local workspaces_root",
			committed: validYAML,
			local:     "paths:\n  workspaces_root: /home/dev/ws\n",
			opts:      LoadOptions{Validate: ValidateFull},
			check: func(t *testing.T, f *schema.File) {
				if f.Paths.WorkspacesRoot != "/home/dev/ws" {
					t.Errorf("merged workspaces_root = %q, want /home/dev/ws", f.Paths.WorkspacesRoot)
				}
			},
		},
		{
			name:      "full rejects relative .local workspaces_root",
			committed: validYAML,
			local:     "paths:\n  workspaces_root: relative/ws\n",
			opts:      LoadOptions{Validate: ValidateFull},
			wantErr:   "validate " + naming.ConfigFileLocal,
		},
		{
			name:      "localonly validates .local but not committed",
			committed: committedWithPaths, // would fail Full, must pass LocalOnly
			local:     "paths:\n  workspaces_root: /ok/abs\n",
			opts:      LoadOptions{Validate: ValidateLocalOnly},
			check: func(t *testing.T, f *schema.File) {
				// .local overrides the committed workspaces_root (scalar wins).
				if f.Paths.WorkspacesRoot != "/ok/abs" {
					t.Errorf("workspaces_root = %q, want /ok/abs", f.Paths.WorkspacesRoot)
				}
			},
		},
		{
			name:      "localonly rejects relative .local workspaces_root",
			committed: validYAML,
			local:     "paths:\n  workspaces_root: relative/ws\n",
			opts:      LoadOptions{Validate: ValidateLocalOnly},
			wantErr:   "validate " + naming.ConfigFileLocal,
		},
		{
			name:      "require cluster passes when cluster.name set",
			committed: validYAML,
			opts:      LoadOptions{RequireCluster: true},
			check: func(t *testing.T, f *schema.File) {
				if f.Cluster.Name != "acme-local" {
					t.Errorf("cluster.name = %q", f.Cluster.Name)
				}
			},
		},
		{
			name:      "require cluster errors when cluster.name empty",
			committed: strings.Replace(minimalYAML, "  name: acme-local\n", "", 1),
			opts:      LoadOptions{RequireCluster: true},
			wantErr:   "cluster.name is required",
		},
		{
			name:      "local scalar override wins and adds new field",
			committed: validYAML,
			local:     "cluster:\n  name: acme-dev\npaths:\n  workspaces_root: /home/dev/ws\n",
			opts:      LoadOptions{Validate: ValidateNone},
			check: func(t *testing.T, f *schema.File) {
				if f.Cluster.Name != "acme-dev" {
					t.Errorf("cluster.name = %q, want acme-dev (.local override)", f.Cluster.Name)
				}
				// Untouched committed fields survive the merge.
				if f.Cluster.Registry != "acme-registry" || f.Cluster.HttpPort != 8080 {
					t.Errorf("committed cluster fields lost: %+v", f.Cluster)
				}
				if f.Paths.WorkspacesRoot != "/home/dev/ws" {
					t.Errorf("workspaces_root = %q", f.Paths.WorkspacesRoot)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeRepo(t, tc.committed, tc.local)
			f, err := Load(dir, tc.opts)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				tc.check(t, f)
			}
		})
	}
}

// TestLoad_MissingCommitted asserts a missing flywheel.yaml is a read error
// regardless of options.
func TestLoad_MissingCommitted(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(dir, LoadOptions{}); err == nil {
		t.Fatal("expected read error for missing flywheel.yaml")
	} else if !strings.Contains(err.Error(), "read "+naming.ConfigFile) {
		t.Fatalf("error = %q, want read %s", err, naming.ConfigFile)
	}
}

// TestLoad_LocalClusterOverrideHonoredConsistently is the cross-command
// divergence guard: `flywheel clean` (readClusterConfig, RequireCluster) used
// to skip the flywheel.yaml.local merge that `flywheel use` honoured, so a
// per-developer cluster-name override was silently ignored. Both now go
// through Load, so a .local cluster.name override is seen under either options
// shape.
func TestLoad_LocalClusterOverrideHonoredConsistently(t *testing.T) {
	dir := writeRepo(t, validYAML, "cluster:\n  name: acme-override\n")
	for _, opts := range []LoadOptions{
		{RequireCluster: true},   // clean / use
		{Validate: ValidateFull}, // up
		{},                       // add app / publish / doctor / down
	} {
		f, err := Load(dir, opts)
		if err != nil {
			t.Fatalf("opts %+v: %v", opts, err)
		}
		if f.Cluster.Name != "acme-override" {
			t.Errorf("opts %+v: cluster.name = %q, want acme-override (.local honoured)", opts, f.Cluster.Name)
		}
	}
}
