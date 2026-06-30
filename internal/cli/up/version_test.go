package up

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	flywheel "github.com/cobr-io/flywheel"
)

const upSampleYAML = `schema: v1alpha1
flywheel:
  version: %s    # pinned tag
client:
  name: acme
`

func setBuildVersion(t *testing.T, v string) {
	t.Helper()
	orig := flywheel.BuildVersion
	flywheel.BuildVersion = v
	t.Cleanup(func() { flywheel.BuildVersion = orig })
}

func setConfirmBump(t *testing.T, accepted, asked bool) {
	t.Helper()
	orig := confirmBump
	confirmBump = func(io.Reader, io.Writer, string) (bool, bool) { return accepted, asked }
	t.Cleanup(func() { confirmBump = orig })
}

func writeUpYAML(t *testing.T, pin string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "flywheel.yaml")
	if err := os.WriteFile(p, []byte(fmt.Sprintf(upSampleYAML, pin)), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertPin(t *testing.T, dir, want string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "flywheel.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "version: "+want) {
		t.Errorf("flywheel.yaml pin = not %q:\n%s", want, b)
	}
}

func TestIsReleaseVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.2.0":           true,
		"v1.0.0":           true,
		"v0.0.0-dev":       false, // unstamped dev sentinel
		"v0.1.0-5-gabc123": false, // git-describe dev build
		"v0.2.0+meta":      false, // build metadata
		"0.2.0":            false, // x/mod/semver requires the v prefix
		"garbage":          false,
		"":                 false,
	} {
		if got := isReleaseVersion(v); got != want {
			t.Errorf("isReleaseVersion(%q) = %v, want %v", v, got, want)
		}
	}
}

// TestCheckVersionDrift_Skips covers the cases that proceed unchanged with no
// file write: dev builds (the rollback-irrelevant case the user dogfoods with),
// an in-sync pin, and an unparseable pin.
func TestCheckVersionDrift_Skips(t *testing.T) {
	cases := []struct {
		name, build, pin string
	}{
		{"unstamped dev build", "v0.0.0-dev", "v0.2.0"},
		{"git-describe dev build behind pin", "v0.1.0-5-gabc123", "v0.2.0"},
		{"in sync", "v0.2.0", "v0.2.0"},
		{"unparseable pin", "v0.2.0", "latest"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			setBuildVersion(t, c.build)
			dir := writeUpYAML(t, c.pin)
			got, err := checkVersionDrift(io.Discard, strings.NewReader(""), dir, c.pin)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.pin {
				t.Errorf("returned version = %q, want unchanged %q", got, c.pin)
			}
			assertPin(t, dir, c.pin) // file untouched
		})
	}
}

// TestCheckVersionDrift_BinaryBehindHardStops is the rollback-safety guarantee:
// an older binary against a newer pin aborts and never rewrites the pin.
func TestCheckVersionDrift_BinaryBehindHardStops(t *testing.T) {
	setBuildVersion(t, "v0.1.0")
	dir := writeUpYAML(t, "v0.2.0")
	got, err := checkVersionDrift(io.Discard, strings.NewReader(""), dir, "v0.2.0")
	if err == nil {
		t.Fatal("expected hard-stop error, got nil")
	}
	if !strings.Contains(err.Error(), "older") {
		t.Errorf("error = %q, want it to mention the binary is older", err)
	}
	if got != "v0.2.0" {
		t.Errorf("returned version = %q, want pin unchanged", got)
	}
	assertPin(t, dir, "v0.2.0") // never downgraded
}

func TestCheckVersionDrift_BinaryAheadNonTTYAborts(t *testing.T) {
	setBuildVersion(t, "v0.3.0")
	dir := writeUpYAML(t, "v0.2.0")
	// strings.Reader is not a *os.File → real confirmBump sees a non-TTY.
	_, err := checkVersionDrift(io.Discard, strings.NewReader("y\n"), dir, "v0.2.0")
	if err == nil {
		t.Fatal("expected non-TTY abort error, got nil")
	}
	assertPin(t, dir, "v0.2.0") // no silent mutation in non-interactive contexts
}

func TestCheckVersionDrift_BinaryAheadDeclined(t *testing.T) {
	setBuildVersion(t, "v0.3.0")
	setConfirmBump(t, false, true) // asked, declined
	dir := writeUpYAML(t, "v0.2.0")
	got, err := checkVersionDrift(io.Discard, strings.NewReader(""), dir, "v0.2.0")
	if err == nil {
		t.Fatal("expected abort-on-decline error, got nil")
	}
	if got != "v0.2.0" {
		t.Errorf("returned version = %q, want pin unchanged", got)
	}
	assertPin(t, dir, "v0.2.0")
}

func TestCheckVersionDrift_BinaryAheadAcceptedBumpsPin(t *testing.T) {
	setBuildVersion(t, "v0.3.0")
	setConfirmBump(t, true, true) // asked, accepted
	dir := writeUpYAML(t, "v0.2.0")
	got, err := checkVersionDrift(io.Discard, strings.NewReader(""), dir, "v0.2.0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "v0.3.0" {
		t.Errorf("returned version = %q, want bumped v0.3.0", got)
	}
	assertPin(t, dir, "v0.3.0") // pin rolled forward
	// Comment preserved by the underlying config writer.
	b, _ := os.ReadFile(filepath.Join(dir, "flywheel.yaml"))
	if !strings.Contains(string(b), "# pinned tag") {
		t.Errorf("lost the inline version comment:\n%s", b)
	}
}
