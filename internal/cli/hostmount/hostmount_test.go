package hostmount

import "testing"

func TestUnshareableTempDir(t *testing.T) {
	cases := []struct {
		in   string
		want string
		bad  bool
	}{
		{"/tmp/acme-gitops", "/tmp", true},
		{"/tmp", "/tmp", true},
		{"/private/tmp/x", "/private/tmp", true},
		{"/var/folders/xx/T/y", "/var/folders", true},
		{"/private/var/folders/z", "/private/var/folders", true},
		{"/Users/dev/src/acme-gitops", "", false},
		{"/Volumes/ext/repo", "", false},
		{"/tmpfoo/bar", "", false}, // not actually under /tmp
		{"/home/dev/work", "", false},
	}
	for _, c := range cases {
		got, bad := UnshareableTempDir(c.in)
		if bad != c.bad || (bad && got != c.want) {
			t.Errorf("UnshareableTempDir(%q) = (%q, %v), want (%q, %v)", c.in, got, bad, c.want, c.bad)
		}
	}
}

func TestGuard_OverrideBypasses(t *testing.T) {
	t.Setenv(AllowEnv, "1")
	if err := Guard("run flywheel up", "/tmp/whatever"); err != nil {
		t.Errorf("override should bypass the guard, got: %v", err)
	}
}

func TestGuard_NonTempPathPasses(t *testing.T) {
	t.Setenv(AllowEnv, "")
	if err := Guard("run flywheel up", "/Users/dev/src/acme-gitops"); err != nil {
		t.Errorf("a home-dir path should pass, got: %v", err)
	}
}
