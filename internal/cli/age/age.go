// Package age wraps filippo.io/age for keypair generation and host-only
// private key storage. The keypair model per design § flywheel new step
// 6: public key into .sops.yaml (committed); private key into
// ~/.config/flywheel/<client>/age.key (0600, never committed).
package age

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"filippo.io/age"
)

// Keypair is what `flywheel new` step 6 generates. PublicKey is the
// recipient-style `age1...` string that goes into .sops.yaml; PrivateKey
// is the `AGE-SECRET-KEY-...` string that lives only on the developer's
// host.
type Keypair struct {
	PublicKey  string
	PrivateKey string
}

// Generator produces age keypairs. The real implementation calls
// `age.GenerateX25519Identity`; tests inject a deterministic generator
// so golden-file tests stay stable across runs.
type Generator interface {
	Generate() (Keypair, error)
}

// RealGenerator is the production Generator that produces a fresh
// X25519 identity per call.
type RealGenerator struct{}

func (RealGenerator) Generate() (Keypair, error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return Keypair{}, fmt.Errorf("generate age identity: %w", err)
	}
	return Keypair{
		PublicKey:  id.Recipient().String(),
		PrivateKey: id.String(),
	}, nil
}

// FixedGenerator returns the same keypair every Generate() call. Tests
// only. Construct with FixedKeypair(kp).
type FixedGenerator struct {
	Pair Keypair
}

func FixedKeypair(kp Keypair) Generator { return FixedGenerator{Pair: kp} }

func (f FixedGenerator) Generate() (Keypair, error) { return f.Pair, nil }

// HostKeyDir returns ~/.config/flywheel/<client>/, the host-only
// directory where the private key lands. Caller is responsible for
// creating it before WritePrivateKey (which mkdir's it anyway).
func HostKeyDir(clientName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "flywheel", clientName), nil
}

// HostKeyPath is ~/.config/flywheel/<client>/age.key.
func HostKeyPath(clientName string) (string, error) {
	dir, err := HostKeyDir(clientName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "age.key"), nil
}

// WritePrivateKey writes the private key to ~/.config/flywheel/<client>/age.key
// with mode 0600. Creates parent dirs (0700) if missing.
//
// Refuses to overwrite an existing file — `init` should call LoadKeypair
// first and only fall through to WritePrivateKey when no key exists. The
// guard remains as a safety net so a buggy caller can't silently replace
// a developer's key (which would orphan any committed SOPS files).
func WritePrivateKey(clientName, privateKey string) (string, error) {
	dir, err := HostKeyDir(clientName)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "age.key")
	return WritePrivateKeyAt(path, privateKey)
}

// WritePrivateKeyAt is WritePrivateKey with an explicit on-disk path
// (tests inject HomeOverride-derived paths). Same overwrite-refusal
// semantics. Creates parent dirs (0700) if missing.
func WritePrivateKeyAt(path, privateKey string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists; refusing to overwrite an existing age key", path)
	}
	if err := os.WriteFile(path, []byte(privateKey+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", path, err)
	}
	return path, nil
}

// LoadKeypair reads ~/.config/flywheel/<client>/age.key, parses the
// stored private key, and returns the full Keypair (private + derived
// public) plus the path it was loaded from.
//
// `flywheel init` calls this first; if it returns fs.ErrNotExist the
// caller falls through to Generator.Generate() + WritePrivateKey. This
// preserves the per-developer identity across destroy/init cycles so
// rendered .sops.yaml keeps matching any committed *.sops.yaml content.
func LoadKeypair(clientName string) (Keypair, string, error) {
	path, err := HostKeyPath(clientName)
	if err != nil {
		return Keypair{}, "", err
	}
	kp, err := LoadKeypairFromPath(path)
	return kp, path, err
}

// LoadKeypairFromPath is LoadKeypair with an explicit on-disk path.
// Returns fs.ErrNotExist (wrapped) when the file is missing, so
// callers can errors.Is to fall through to keypair generation.
func LoadKeypairFromPath(path string) (Keypair, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Keypair{}, err
	}
	line := strings.TrimSpace(string(raw))
	id, err := age.ParseX25519Identity(line)
	if err != nil {
		return Keypair{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return Keypair{
		PublicKey:  id.Recipient().String(),
		PrivateKey: id.String(),
	}, nil
}

// ReadPrivateKey loads the private key from
// ~/.config/flywheel/<client>/age.key. Returns the raw text (suitable
// for sops/age consumers) and the path it was read from (for error
// messages). Caller is responsible for the mode-check elsewhere if
// needed — design § up step 5 mode-checks 0600.
func ReadPrivateKey(clientName string) (string, string, error) {
	path, err := HostKeyPath(clientName)
	if err != nil {
		return "", "", err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", path, fmt.Errorf("read %s: %w", path, err)
	}
	return string(raw), path, nil
}

// CheckMode verifies the file at `path` has mode 0600 exactly. The
// design's invariant is that the private key is host-only and not
// world-readable; a mode wider than 0600 is a setup bug.
func CheckMode(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("%s has mode %o; want 0600 (host-only)", path, info.Mode().Perm())
	}
	return nil
}
