package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/spf13/cobra"
)

// TestCommandTree asserts the full set of subcommands is registered and that
// the retired `new` command is gone (it falls through to cobra's unknown-command
// handling now).
func TestCommandTree(t *testing.T) {
	root := newRootCmd()
	got := map[string]bool{}
	for _, c := range root.Commands() {
		got[c.Name()] = true
	}
	want := []string{
		"doctor", "init", "up", "down", "clean",
		"add", "version",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("command %q not registered", w)
		}
	}
	if got["add-app"] {
		t.Errorf("retired flat `add-app` command should not be registered (now `add app`)")
	}
	if got["new"] {
		t.Errorf("retired `new` command should not be registered")
	}
	// `app` is a child of `add`, not a top-level command.
	if subCmd(root, "add", "app") == nil {
		t.Error("`add app` subcommand not registered")
	}
	// Note: cobra adds `completion` lazily during Execute(), so it won't appear
	// in Commands() here; a behavioral check (`flywheel completion zsh`) covers it.
}

// TestCommandFlags pins each command's flag surface so the migration didn't drop
// or rename a flag.
func TestCommandFlags(t *testing.T) {
	root := newRootCmd()
	cmds := commandsByName(root)
	cases := map[string][]string{
		"doctor": {"quick"},
		"init":   {"org", "version"},
		"up":     {"yes", "yes-additive", "wait"},
		"down":   {"yes"},
		"clean":  {"orphaned"},
	}
	for name, flags := range cases {
		cmd, ok := cmds[name]
		if !ok {
			t.Errorf("command %q missing", name)
			continue
		}
		for _, f := range flags {
			if cmd.Flags().Lookup(f) == nil {
				t.Errorf("command %q missing flag --%s", name, f)
			}
		}
	}
	// `add app`'s flags live on the child command.
	addApp := subCmd(root, "add", "app")
	if addApp == nil {
		t.Fatal("`add app` subcommand missing")
	}
	for _, f := range []string{"name", "image", "context", "dockerfile"} {
		if addApp.Flags().Lookup(f) == nil {
			t.Errorf("`add app` missing flag --%s", f)
		}
	}
}

// TestPersistentGlobals asserts the two globals are persistent (so they work
// before or after the subcommand) with the -v shorthand.
func TestPersistentGlobals(t *testing.T) {
	root := newRootCmd()
	if root.PersistentFlags().Lookup("no-color") == nil {
		t.Error("--no-color is not a persistent flag")
	}
	v := root.PersistentFlags().Lookup("verbose")
	if v == nil {
		t.Fatal("--verbose is not a persistent flag")
	}
	if v.Shorthand != "v" {
		t.Errorf("--verbose shorthand = %q, want v", v.Shorthand)
	}
}

func TestArgsValidation(t *testing.T) {
	root := newRootCmd()
	cmds := commandsByName(root)
	// `add app` requires at least one positional (the app name / worktree).
	addApp := subCmd(root, "add", "app")
	if addApp == nil {
		t.Fatal("`add app` subcommand missing")
	}
	if err := addApp.Args(addApp, nil); err == nil {
		t.Error("`add app` should reject zero args")
	}
	if err := addApp.Args(addApp, []string{"app"}); err != nil {
		t.Errorf("`add app` should accept one arg, got %v", err)
	}
	// init takes an optional single positional.
	if err := cmds["init"].Args(cmds["init"], []string{"a", "b"}); err == nil {
		t.Error("init should reject two args")
	}
}

func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"explicit usage", exitError{code: 2, err: errSilent}, 2},
		{"explicit stub", exitError{code: 1, err: errSilent}, 1},
		{"unknown command", errors.New(`unknown command "x" for "flywheel"`), 2},
		{"runtime error", errors.New("step 7: boom"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCodeFor(tc.err); got != tc.want {
				t.Errorf("exitCodeFor = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestBadFlagIsUsageError drives Execute with an unknown flag and asserts the
// FlagErrorFunc tags it as a usage error (exit 2), short-circuiting before the
// command's RunE (no side effects).
func TestBadFlagIsUsageError(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"up", "--bogus-flag"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error for an unknown flag")
	}
	if got := exitCodeFor(err); got != 2 {
		t.Errorf("bad flag exit code = %d, want 2", got)
	}
}

func commandsByName(root *cobra.Command) map[string]*cobra.Command {
	m := map[string]*cobra.Command{}
	for _, c := range root.Commands() {
		m[c.Name()] = c
	}
	return m
}

// subCmd walks the command tree by name (e.g. subCmd(root, "add", "app")),
// returning nil if any segment is missing.
func subCmd(cmd *cobra.Command, names ...string) *cobra.Command {
	for _, name := range names {
		var next *cobra.Command
		for _, c := range cmd.Commands() {
			if c.Name() == name {
				next = c
				break
			}
		}
		if next == nil {
			return nil
		}
		cmd = next
	}
	return cmd
}
