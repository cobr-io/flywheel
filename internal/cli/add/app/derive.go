package app

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	gopath "path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
	"golang.org/x/mod/modfile"
)

// manifestNamer extracts a raw (un-sanitised) project name from one kind of
// project manifest in a worktree directory. found=false means the manifest is
// absent (skip it); a non-nil error (malformed file) is treated by deriveName as
// "no name from this manifest".
type manifestNamer struct {
	label string
	read  func(dir string) (name string, found bool, err error)
}

// manifests is the scan set, in a stable order. The order only affects which
// file is reported when several manifests agree on a name; conflicting names are
// an error, never resolved by precedence.
var manifests = []manifestNamer{
	{"package.json", jsonNameField("package.json")},
	{"pyproject.toml", readPyproject},
	{"setup.cfg", readSetupCfg},
	{"go.mod", readGoMod},
	{"Cargo.toml", readCargoToml},
	{"composer.json", jsonNameField("composer.json")},
	{"pom.xml", readPomXML},
	{"*.gemspec", readGemspec},
}

// deriveName inspects a worktree directory and returns the app name to use.
//
//   - no recognised manifest (or none with a name) → ("", "", nil); the caller
//     falls back to the directory name.
//   - exactly one distinct sanitised name (one manifest, or several agreeing) →
//     (name, reportingLabel, nil).
//   - two or more manifests declaring different names → ("", "", error); the
//     caller must pass --name.
func deriveName(dir string) (name, source string, err error) {
	type hit struct{ label, name string }
	var hits []hit
	for _, m := range manifests {
		raw, found, rerr := m.read(dir)
		if !found || rerr != nil {
			continue
		}
		if s := sanitizeName(raw); s != "" {
			hits = append(hits, hit{m.label, s})
		}
	}

	distinct := map[string]string{} // name → first label that yielded it
	var order []string
	for _, h := range hits {
		if _, seen := distinct[h.name]; !seen {
			distinct[h.name] = h.label
			order = append(order, h.name)
		}
	}

	switch len(distinct) {
	case 0:
		return "", "", nil
	case 1:
		n := order[0]
		return n, distinct[n], nil
	default:
		var b strings.Builder
		b.WriteString("project manifests declare different names — pass --name to choose:")
		for _, h := range hits {
			fmt.Fprintf(&b, "\n  %s: %s", h.label, h.name)
		}
		return "", "", errors.New(b.String())
	}
}

var invalidNameChars = regexp.MustCompile(`[^a-z0-9]+`)

// sanitizeName reduces an arbitrary project name to a DNS-1123 label: lowercase,
// scope/vendor prefix dropped, invalid runs collapsed to '-', trimmed, ≤63
// chars. Returns "" when nothing usable remains.
func sanitizeName(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	// Drop an npm scope (@scope/name) or composer vendor (vendor/name): the last
	// path segment is the package identity.
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	s = invalidNameChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.Trim(s[:63], "-")
	}
	return s
}

// readManifest reads dir/file, reporting found=false (nil error) when absent.
func readManifest(dir, file string) (data []byte, found bool, err error) {
	data, err = os.ReadFile(filepath.Join(dir, file))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// jsonNameField reads the top-level "name" string from a JSON manifest
// (package.json, composer.json).
func jsonNameField(file string) func(string) (string, bool, error) {
	return func(dir string) (string, bool, error) {
		data, found, err := readManifest(dir, file)
		if !found || err != nil {
			return "", found, err
		}
		var m struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &m); err != nil {
			return "", true, err
		}
		return m.Name, true, nil
	}
}

// readPyproject reads [project].name, falling back to [tool.poetry].name.
func readPyproject(dir string) (string, bool, error) {
	data, found, err := readManifest(dir, "pyproject.toml")
	if !found || err != nil {
		return "", found, err
	}
	var m struct {
		Project struct {
			Name string `toml:"name"`
		} `toml:"project"`
		Tool struct {
			Poetry struct {
				Name string `toml:"name"`
			} `toml:"poetry"`
		} `toml:"tool"`
	}
	if err := toml.Unmarshal(data, &m); err != nil {
		return "", true, err
	}
	if m.Project.Name != "" {
		return m.Project.Name, true, nil
	}
	return m.Tool.Poetry.Name, true, nil
}

// readSetupCfg reads name= from the [metadata] section of a setuptools cfg.
func readSetupCfg(dir string) (string, bool, error) {
	data, found, err := readManifest(dir, "setup.cfg")
	if !found || err != nil {
		return "", found, err
	}
	section := ""
	for _, line := range strings.Split(string(data), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			section = strings.ToLower(strings.Trim(t, "[]"))
			continue
		}
		if section == "metadata" {
			if k, v, ok := strings.Cut(t, "="); ok && strings.TrimSpace(k) == "name" {
				return strings.TrimSpace(v), true, nil
			}
		}
	}
	return "", true, nil
}

// readCargoToml reads [package].name.
func readCargoToml(dir string) (string, bool, error) {
	data, found, err := readManifest(dir, "Cargo.toml")
	if !found || err != nil {
		return "", found, err
	}
	var m struct {
		Package struct {
			Name string `toml:"name"`
		} `toml:"package"`
	}
	if err := toml.Unmarshal(data, &m); err != nil {
		return "", true, err
	}
	return m.Package.Name, true, nil
}

// readPomXML reads the project-level <artifactId> (a nested <parent><artifactId>
// stays bound to the parent struct, so it won't leak in).
func readPomXML(dir string) (string, bool, error) {
	data, found, err := readManifest(dir, "pom.xml")
	if !found || err != nil {
		return "", found, err
	}
	var m struct {
		ArtifactID string `xml:"artifactId"`
	}
	if err := xml.Unmarshal(data, &m); err != nil {
		return "", true, err
	}
	return m.ArtifactID, true, nil
}

// gemspecName matches `<var>.name = "foo"` (or single quotes) in a gemspec.
var gemspecName = regexp.MustCompile(`(?m)^\s*\w+\.name\s*=\s*["']([^"']+)["']`)

// readGemspec reads the name from the first *.gemspec in dir. Ruby isn't parsed;
// a regex for the conventional `spec.name = "..."` assignment suffices.
func readGemspec(dir string) (string, bool, error) {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.gemspec"))
	if len(matches) == 0 {
		return "", false, nil
	}
	sort.Strings(matches)
	data, err := os.ReadFile(matches[0])
	if err != nil {
		return "", true, err
	}
	if m := gemspecName.FindSubmatch(data); m != nil {
		return string(m[1]), true, nil
	}
	return "", true, nil
}

var goModVersionSuffix = regexp.MustCompile(`^v[0-9]+$`)

// readGoMod returns the final element of the module path, skipping a /vN
// major-version suffix (github.com/x/foo/v2 → foo).
func readGoMod(dir string) (string, bool, error) {
	data, found, err := readManifest(dir, "go.mod")
	if !found || err != nil {
		return "", found, err
	}
	mod, err := modfile.Parse("go.mod", data, nil)
	if err != nil || mod.Module == nil {
		return "", true, fmt.Errorf("parse go.mod: %w", err)
	}
	p := mod.Module.Mod.Path
	base := gopath.Base(p)
	if goModVersionSuffix.MatchString(base) {
		base = gopath.Base(gopath.Dir(p))
	}
	return base, true, nil
}
