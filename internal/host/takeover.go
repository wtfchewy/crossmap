package host

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// System-mode paths.
const (
	// SystemSocket is where every usbmuxd-aware tool on macOS looks.
	SystemSocket = "/var/run/usbmuxd"
	// StashedSocket is where Crossmap moves Apple's socket while it stands
	// in. Renaming a unix socket moves the live listening binding with it,
	// so launchd's usbmuxd keeps serving at the new path and Crossmap can
	// still reach local devices through it.
	StashedSocket = "/var/run/usbmuxd.crossmap-orig"
	// StateFile records an in-progress takeover so a Crossmap that is
	// killed without unwinding can be repaired rather than leaving the
	// machine without a usbmuxd.
	StateFile = "/var/run/crossmap-takeover.json"
)

// TakeoverState is the on-disk record of a live takeover.
type TakeoverState struct {
	OriginalPath string    `json:"original_path"`
	StashedPath  string    `json:"stashed_path"`
	PID          int       `json:"pid"`
	StartedAt    time.Time `json:"started_at"`
}

// ErrTakeoverInProgress reports that a previous takeover was never unwound.
var ErrTakeoverInProgress = errors.New(
	"host: a previous Crossmap takeover was not unwound; run `crossmap repair` to restore /var/run/usbmuxd")

// socketAlive reports whether something is accepting on a unix socket path.
func socketAlive(path string) bool {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// TakeOver moves Apple's usbmuxd socket aside so Crossmap can bind the path
// every tool expects. It returns the path the real daemon now answers on,
// plus a restore function that must be called before exit.
//
// This requires root and changes machine-wide state, so it validates
// aggressively and refuses rather than guessing.
func TakeOver() (stashed string, restore func() error, err error) {
	if os.Geteuid() != 0 {
		return "", nil, errors.New("host: system mode requires root (re-run with sudo)")
	}

	// A leftover stash means a previous run died mid-takeover. Unwinding
	// automatically could destroy whatever is at the live path, so stop.
	if _, err := os.Stat(StashedSocket); err == nil {
		return "", nil, ErrTakeoverInProgress
	}

	fi, err := os.Stat(SystemSocket)
	if err != nil {
		return "", nil, fmt.Errorf("host: %s is missing; is usbmuxd running?: %w", SystemSocket, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return "", nil, fmt.Errorf("host: %s is not a socket; refusing to touch it", SystemSocket)
	}
	if !socketAlive(SystemSocket) {
		return "", nil, fmt.Errorf("host: nothing is accepting on %s; refusing to move a dead socket", SystemSocket)
	}

	if err := os.Rename(SystemSocket, StashedSocket); err != nil {
		return "", nil, fmt.Errorf("host: move %s aside: %w", SystemSocket, err)
	}

	// Confirm the real daemon followed the rename before anything starts
	// depending on it; if it did not, put it straight back.
	if !socketAlive(StashedSocket) {
		_ = os.Rename(StashedSocket, SystemSocket)
		return "", nil, fmt.Errorf("host: usbmuxd did not follow the rename to %s; nothing was changed", StashedSocket)
	}

	state := TakeoverState{
		OriginalPath: SystemSocket,
		StashedPath:  StashedSocket,
		PID:          os.Getpid(),
		StartedAt:    time.Now(),
	}
	if err := writeState(state); err != nil {
		_ = os.Rename(StashedSocket, SystemSocket)
		return "", nil, err
	}

	var done bool
	restore = func() error {
		if done {
			return nil
		}
		done = true
		return Restore()
	}
	return StashedSocket, restore, nil
}

// Restore puts Apple's usbmuxd socket back at its canonical path. It is safe
// to call when no takeover is active.
func Restore() error {
	state, err := readState()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Nothing recorded, but a stash may still exist from an older
			// build or a partially written state file.
			if _, serr := os.Stat(StashedSocket); serr == nil {
				state = TakeoverState{OriginalPath: SystemSocket, StashedPath: StashedSocket}
			} else {
				return nil
			}
		} else {
			return err
		}
	}

	if _, err := os.Stat(state.StashedPath); err != nil {
		// Nothing to put back; clear the record so we stop warning.
		_ = os.Remove(StateFile)
		return fmt.Errorf("host: stashed socket %s is gone; a reboot will recreate %s: %w",
			state.StashedPath, state.OriginalPath, err)
	}

	// Remove Crossmap's stand-in socket, but never a live one that is not
	// ours to delete.
	if fi, err := os.Stat(state.OriginalPath); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("host: %s is not a socket; refusing to remove it", state.OriginalPath)
		}
		if err := os.Remove(state.OriginalPath); err != nil {
			return fmt.Errorf("host: remove stand-in socket: %w", err)
		}
	}

	if err := os.Rename(state.StashedPath, state.OriginalPath); err != nil {
		return fmt.Errorf("host: restore %s: %w", state.OriginalPath, err)
	}
	_ = os.Remove(StateFile)
	return nil
}

// ActiveTakeover reports the recorded takeover, if any.
func ActiveTakeover() (TakeoverState, bool) {
	st, err := readState()
	if err != nil {
		return TakeoverState{}, false
	}
	return st, true
}

func writeState(s TakeoverState) error {
	body, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := StateFile + ".tmp"
	if err := os.MkdirAll(filepath.Dir(StateFile), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("host: write takeover state: %w", err)
	}
	if err := os.Rename(tmp, StateFile); err != nil {
		return fmt.Errorf("host: commit takeover state: %w", err)
	}
	return nil
}

func readState() (TakeoverState, error) {
	body, err := os.ReadFile(StateFile)
	if err != nil {
		return TakeoverState{}, err
	}
	var s TakeoverState
	if err := json.Unmarshal(body, &s); err != nil {
		return TakeoverState{}, fmt.Errorf("host: parse takeover state: %w", err)
	}
	return s, nil
}
