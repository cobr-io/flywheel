package schema

import (
	"strings"
	"testing"
)

// T0.1 — validates a representative flywheel.yaml and rejects a malformed one.

const validYAML = `
schema: v1alpha1
flywheel:
  version: v0.1.0
client:
  name: acme
  org: cobr-io
cluster:
  name: acme-local
  registry: acme-local-registry
  registry_port: 50001
  http_port: 8083
  https_port: 8543
  servers: 1
  agents: 2
  k3s_image: v1.34.1-k3s1
namespaces:
  flywheel: flywheel-system
  apps: apps
flux:
  interval_local: 10s
  iac_interval: 30s
local:
  domain: localdev.me
sops:
  age_recipients_local:
    - age1qx5vhc3z8q
`

func TestParse_ValidYAML(t *testing.T) {
	f, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if f.Schema != "v1alpha1" {
		t.Errorf("schema = %q, want v1alpha1", f.Schema)
	}
	if f.Cluster.RegistryPort != 50001 {
		t.Errorf("registry_port = %d, want 50001", f.Cluster.RegistryPort)
	}
}

func TestValidate_FullyPopulated(t *testing.T) {
	f, err := Parse([]byte(validYAML))
	if err != nil {
		t.Fatal(err)
	}
	if err := Validate(f); err != nil {
		t.Fatalf("Validate on valid file: %v", err)
	}
}

func TestIntegrationBranch_Default(t *testing.T) {
	f, err := Parse([]byte(validYAML)) // no git block
	if err != nil {
		t.Fatal(err)
	}
	if got := f.IntegrationBranch(); got != "main" {
		t.Errorf("IntegrationBranch() = %q, want main (default)", got)
	}
}

func TestIntegrationBranch_Configured(t *testing.T) {
	f, err := Parse([]byte(validYAML + "git:\n  integration_branch: develop\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.IntegrationBranch(); got != "develop" {
		t.Errorf("IntegrationBranch() = %q, want develop", got)
	}
	if err := Validate(f); err != nil {
		t.Errorf("a valid integration_branch should pass Validate: %v", err)
	}
}

// A present but implausible branch name is rejected (a typo here would
// silently disable the local-only guard). Empty/absent falls back to main.
func TestValidate_RejectsBadIntegrationBranch(t *testing.T) {
	f, err := Parse([]byte(validYAML + "git:\n  integration_branch: \"bad branch\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = Validate(f)
	if err == nil || !strings.Contains(err.Error(), "git.integration_branch") {
		t.Fatalf("expected a git.integration_branch validation error, got %v", err)
	}
}

func TestGitServerMemoryLimit_Default(t *testing.T) {
	f, err := Parse([]byte(validYAML)) // no git_server block
	if err != nil {
		t.Fatal(err)
	}
	if got := f.GitServerMemoryLimit(); got != DefaultGitServerMemoryLimit {
		t.Errorf("GitServerMemoryLimit() = %q, want %q (default)", got, DefaultGitServerMemoryLimit)
	}
}

func TestGitServerMemoryLimit_Configured(t *testing.T) {
	f, err := Parse([]byte(validYAML + "git_server:\n  memory_limit: 512Mi\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.GitServerMemoryLimit(); got != "512Mi" {
		t.Errorf("GitServerMemoryLimit() = %q, want 512Mi", got)
	}
	if err := Validate(f); err != nil {
		t.Errorf("a valid memory_limit should pass Validate: %v", err)
	}
}

// A present but implausible memory quantity is rejected before it reaches the
// Deployment apply, where it would fail with a less obvious error.
func TestValidate_RejectsBadMemoryLimit(t *testing.T) {
	for _, bad := range []string{"512", "512 Mi", "lots", "512MiB"} {
		f, err := Parse([]byte(validYAML + "git_server:\n  memory_limit: \"" + bad + "\"\n"))
		if err != nil {
			t.Fatal(err)
		}
		err = Validate(f)
		if err == nil || !strings.Contains(err.Error(), "git_server.memory_limit") {
			t.Errorf("memory_limit %q: expected a git_server.memory_limit error, got %v", bad, err)
		}
	}
}

func TestValidate_AcceptsGoodMemoryLimits(t *testing.T) {
	for _, ok := range []string{"128Mi", "512Mi", "1Gi", "256M", "2G"} {
		f, err := Parse([]byte(validYAML + "git_server:\n  memory_limit: " + ok + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		if err := Validate(f); err != nil {
			t.Errorf("memory_limit %q should be accepted, got %v", ok, err)
		}
	}
}

func TestValidate_RejectsMissingFields(t *testing.T) {
	// Drop each required field, assert the error mentions the right field.
	cases := []struct {
		name      string
		drop      string
		wantField string
	}{
		{"no schema", `schema: v1alpha1`, "schema"},
		{"no flywheel.version", `version: v0.1.0`, "flywheel.version"},
		{"no client.name", `name: acme`, "client.name"},
		{"no cluster.name", `name: acme-local`, "cluster.name"},
		{"no cluster.registry", `registry: acme-local-registry`, "cluster.registry"},
		{"no cluster.registry_port", `registry_port: 50001`, "cluster.registry_port"},
		{"no cluster.http_port", `http_port: 8083`, "cluster.http_port"},
		{"no cluster.https_port", `https_port: 8543`, "cluster.https_port"},
		{"no namespaces.flywheel", `flywheel: flywheel-system`, "namespaces.flywheel"},
		{"no namespaces.apps", `apps: apps`, "namespaces.apps"},
		{"no flux.interval_local", `interval_local: 10s`, "flux.interval_local"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stripped := strings.Replace(validYAML, "  "+tc.drop, "", 1)
			// Some drops apply to the same line at top-level: also try the unindented form.
			stripped = strings.Replace(stripped, tc.drop+"\n", "", 1)

			f, err := Parse([]byte(stripped))
			if err != nil {
				t.Fatalf("Parse stripped: %v", err)
			}
			err = Validate(f)
			if err == nil {
				t.Fatalf("Validate on missing %s succeeded, want error", tc.drop)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error %q does not mention %q", err.Error(), tc.wantField)
			}
		})
	}
}

func TestValidate_RejectsWrongSchema(t *testing.T) {
	f := &File{
		Schema:     "v2",
		Flywheel:   Flywheel{Version: "v0.1.0"},
		Client:     Client{Name: "acme"},
		Cluster:    Cluster{Name: "x", Registry: "y", RegistryPort: 1, HttpPort: 2, HttpsPort: 3},
		Namespaces: Namespaces{Flywheel: "f", Apps: "a"},
		Flux:       Flux{IntervalLocal: "10s"},
	}
	err := Validate(f)
	if err == nil {
		t.Fatal("Validate accepted schema=v2; want error")
	}
	if !strings.Contains(err.Error(), "v1alpha1") {
		t.Errorf("error %q should mention the native label", err.Error())
	}
}

func TestValidate_RejectsPathsInCommittedFile(t *testing.T) {
	f, _ := Parse([]byte(validYAML))
	f.Paths.WorkspacesRoot = "/Users/dev/src"
	err := Validate(f)
	if err == nil {
		t.Fatal("Validate accepted paths.workspaces_root in committed file")
	}
	if !strings.Contains(err.Error(), "paths.workspaces_root") {
		t.Errorf("error %q should mention paths.workspaces_root", err.Error())
	}
}

func TestValidateLocal_AbsolutePathRequired(t *testing.T) {
	f := &File{Paths: Paths{WorkspacesRoot: "relative/path"}}
	err := ValidateLocal(f)
	if err == nil {
		t.Fatal("ValidateLocal accepted relative path")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error %q should mention absolute", err.Error())
	}
}

func TestValidateLocal_AbsolutePathAccepted(t *testing.T) {
	f := &File{Paths: Paths{WorkspacesRoot: "/Users/dev/src"}}
	if err := ValidateLocal(f); err != nil {
		t.Fatalf("ValidateLocal: %v", err)
	}
}

func TestValidateLocal_EmptyIsFine(t *testing.T) {
	f := &File{}
	if err := ValidateLocal(f); err != nil {
		t.Fatalf("ValidateLocal on empty: %v", err)
	}
}

func TestValidate_FlywheelImages_AcceptsKnownKeys(t *testing.T) {
	f, _ := Parse([]byte(validYAML))
	f.Flywheel.Images = map[string]string{
		"git-server":               "local/git-server:dev",
		"git-auto-sync":            "local/git-auto-sync:dev",
		"image-builder-controller": "local/image-builder-controller:dev",
	}
	if err := Validate(f); err != nil {
		t.Fatalf("Validate rejected valid images map: %v", err)
	}
}

func TestValidate_FlywheelImages_PartialIsFine(t *testing.T) {
	// Only one override — the other two fall back to ghcr.io defaults
	// at up time. Validate should accept it.
	f, _ := Parse([]byte(validYAML))
	f.Flywheel.Images = map[string]string{
		"git-server": "local/git-server:dev",
	}
	if err := Validate(f); err != nil {
		t.Fatalf("Validate rejected partial images map: %v", err)
	}
}

func TestValidate_FlywheelImages_RejectsUnknownKey(t *testing.T) {
	f, _ := Parse([]byte(validYAML))
	f.Flywheel.Images = map[string]string{"git-typo": "bogus:1"}
	err := Validate(f)
	if err == nil {
		t.Fatal("Validate accepted unknown images key; should reject")
	}
	if !strings.Contains(err.Error(), "git-typo") {
		t.Errorf("error %q should name the bad key", err.Error())
	}
}

func TestValidate_FlywheelImages_RejectsEmptyValue(t *testing.T) {
	f, _ := Parse([]byte(validYAML))
	f.Flywheel.Images = map[string]string{"git-server": ""}
	err := Validate(f)
	if err == nil {
		t.Fatal("Validate accepted empty images value; should reject")
	}
}

// --- workspace block (2026-06-17 addendum) ---

const workspaceYAML = validYAML + `workspace:
  repos:
    - name: sample-app
      url: git@github.com:example-org/sample-app.git
      branch: develop
    - name: hello-py
      local_only: true
`

func TestParse_WorkspaceBlock(t *testing.T) {
	f, err := Parse([]byte(workspaceYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(f.Workspace.Repos); got != 2 {
		t.Fatalf("len(Workspace.Repos) = %d, want 2", got)
	}
	if err := Validate(f); err != nil {
		t.Fatalf("a valid workspace block should pass Validate: %v", err)
	}
}

func TestWorkspaceRepo_LookupAndLocalOnly(t *testing.T) {
	f, _ := Parse([]byte(workspaceYAML))

	r, ok := f.WorkspaceRepo("sample-app")
	if !ok || r.URL == "" || r.LocalOnly {
		t.Errorf("WorkspaceRepo(sample-app) = %+v, %v; want a remote-backed entry", r, ok)
	}
	if r.Branch != "develop" {
		t.Errorf("WorkspaceRepo(sample-app).Branch = %q, want \"develop\"", r.Branch)
	}
	if _, ok := f.WorkspaceRepo("nope"); ok {
		t.Error("WorkspaceRepo(nope) should report ok=false")
	}
	if got := f.LocalOnlyWorktrees(); len(got) != 1 || got[0] != "hello-py" {
		t.Errorf("LocalOnlyWorktrees() = %v, want [hello-py]", got)
	}
}

func TestValidate_WorkspaceRejects(t *testing.T) {
	cases := []struct {
		name  string
		block string
		want  string
	}{
		{"both url and local_only", "workspace:\n  repos:\n    - name: a\n      url: git@x:y.git\n      local_only: true\n", "exactly one"},
		{"neither url nor local_only", "workspace:\n  repos:\n    - name: a\n", "exactly one"},
		{"empty name", "workspace:\n  repos:\n    - url: git@x:y.git\n", "name"},
		{"bad name", "workspace:\n  repos:\n    - name: \"has space\"\n      local_only: true\n", "valid worktree"},
		{"duplicate name", "workspace:\n  repos:\n    - name: a\n      local_only: true\n    - name: a\n      url: git@x:y.git\n", "duplicate"},
		{"bad branch", "workspace:\n  repos:\n    - name: a\n      url: git@x:y.git\n      branch: \"has space\"\n", "branch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := Parse([]byte(validYAML + tc.block))
			if err != nil {
				t.Fatal(err)
			}
			err = Validate(f)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}
