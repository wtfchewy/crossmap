// Package config locates and manages Crossmap's on-disk state, which is just
// the pre-shared key both sides of a link must agree on.
package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// pskBytes is the length of a generated key. 32 bytes is comfortably beyond
// what an HMAC-SHA256 handshake needs.
const pskBytes = 32

// Dir returns Crossmap's configuration directory.
//
// When the host runs under sudo for system mode, HOME still points at the
// invoking user's home on macOS, but SUDO_USER is honoured explicitly so the
// key is read from the same place whether or not the command was elevated.
func Dir() (string, error) {
	if user := os.Getenv("SUDO_USER"); user != "" && os.Geteuid() == 0 {
		if home, err := homeOf(user); err == nil {
			return filepath.Join(home, ".config", "crossmap"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("config: locate home directory: %w", err)
	}
	return filepath.Join(home, ".config", "crossmap"), nil
}

func homeOf(user string) (string, error) {
	out, err := exec.Command("dscl", ".", "-read", "/Users/"+user, "NFSHomeDirectory").Output()
	if err != nil {
		return "", err
	}
	_, home, ok := strings.Cut(strings.TrimSpace(string(out)), ":")
	if !ok {
		return "", errors.New("config: unexpected dscl output")
	}
	return strings.TrimSpace(home), nil
}

// PSKPath returns the default key location.
func PSKPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "psk"), nil
}

// LoadPSK reads the key at path, or at the default location when path is
// empty. The key may also be supplied through CROSSMAP_PSK, which suits
// launchd and CI where a file is awkward.
func LoadPSK(path string) ([]byte, error) {
	if env := os.Getenv("CROSSMAP_PSK"); env != "" {
		return []byte(strings.TrimSpace(env)), nil
	}
	if path == "" {
		var err error
		path, err = PSKPath()
		if err != nil {
			return nil, err
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("config: no pre-shared key at %s; run `crossmap init` to create one", path)
		}
		return nil, fmt.Errorf("config: read key: %w", err)
	}
	key := strings.TrimSpace(string(body))
	if key == "" {
		return nil, fmt.Errorf("config: the key at %s is empty", path)
	}
	return []byte(key), nil
}

// GeneratePSK creates a new key at path, refusing to overwrite an existing
// one so a second `crossmap init` cannot silently break a working pair.
func GeneratePSK(path string) (string, error) {
	if path == "" {
		var err error
		path, err = PSKPath()
		if err != nil {
			return "", err
		}
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("config: a key already exists at %s; delete it first to generate a new one", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("config: create config directory: %w", err)
	}
	raw := make([]byte, pskBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	key := base64.RawURLEncoding.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("config: write key: %w", err)
	}
	// A key written under sudo must stay readable by the real user.
	if user := os.Getenv("SUDO_USER"); user != "" && os.Geteuid() == 0 {
		_ = exec.Command("chown", "-R", user, filepath.Dir(path)).Run()
	}
	return key, nil
}

// WritePSK stores a key supplied by the user, typically copied from the other
// machine's `crossmap init` output.
func WritePSK(path, key string) error {
	if path == "" {
		var err error
		path, err = PSKPath()
		if err != nil {
			return err
		}
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("config: refusing to write an empty key")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: create config directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(key+"\n"), 0o600); err != nil {
		return fmt.Errorf("config: write key: %w", err)
	}
	if user := os.Getenv("SUDO_USER"); user != "" && os.Geteuid() == 0 {
		_ = exec.Command("chown", "-R", user, filepath.Dir(path)).Run()
	}
	return nil
}

// DefaultSocketPath is where the host serves its usbmuxd stand-in when it is
// not taking over the system path. Tools reach it via USBMUXD_SOCKET_ADDRESS.
func DefaultSocketPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "usbmuxd.sock"), nil
}
