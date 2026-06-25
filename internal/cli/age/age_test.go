package age

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRealGenerator_ProducesValidLookingKeypair(t *testing.T) {
	kp, err := (RealGenerator{}).Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(kp.PublicKey, "age1") {
		t.Errorf("public key should start with age1, got %q", kp.PublicKey)
	}
	if !strings.HasPrefix(kp.PrivateKey, "AGE-SECRET-KEY-") {
		t.Errorf("private key should start with AGE-SECRET-KEY-, got %q", kp.PrivateKey)
	}
}

func TestFixedGenerator_DeterministicForTests(t *testing.T) {
	want := Keypair{PublicKey: "age1test", PrivateKey: "AGE-SECRET-KEY-TEST"}
	g := FixedKeypair(want)
	got1, _ := g.Generate()
	got2, _ := g.Generate()
	if got1 != want || got2 != want {
		t.Errorf("FixedGenerator drift: %+v, %+v want %+v", got1, got2, want)
	}
}

func TestWritePrivateKey_Mode0600(t *testing.T) {
	// Redirect HOME for test isolation.
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	path, err := WritePrivateKey("acme", "AGE-SECRET-KEY-TEST")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(tmp, ".config", "flywheel", "acme", "age.key")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 0600", info.Mode().Perm())
	}
}

func TestWritePrivateKey_RefusesOverwrite(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if _, err := WritePrivateKey("acme", "AGE-SECRET-KEY-FIRST"); err != nil {
		t.Fatal(err)
	}
	if _, err := WritePrivateKey("acme", "AGE-SECRET-KEY-SECOND"); err == nil {
		t.Fatal("second WritePrivateKey should fail to avoid silent overwrite")
	}
}

func TestReadPrivateKey_RoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	written := "AGE-SECRET-KEY-ROUND-TRIP-TEST"
	if _, err := WritePrivateKey("acme", written); err != nil {
		t.Fatal(err)
	}
	got, _, err := ReadPrivateKey("acme")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != written {
		t.Errorf("read %q, want %q", got, written)
	}
}

func TestCheckMode_AcceptsExactly0600(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "k")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckMode(p); err != nil {
		t.Errorf("CheckMode on 0600 file failed: %v", err)
	}
}

func TestCheckMode_RejectsWiderModes(t *testing.T) {
	tmp := t.TempDir()
	for _, mode := range []os.FileMode{0o644, 0o660, 0o666} {
		p := filepath.Join(tmp, "k")
		_ = os.Remove(p)
		if err := os.WriteFile(p, []byte("x"), mode); err != nil {
			t.Fatal(err)
		}
		if err := CheckMode(p); err == nil {
			t.Errorf("CheckMode accepted mode %o; should refuse", mode)
		}
	}
}
