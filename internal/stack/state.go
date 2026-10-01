package stack

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Paths under the project root.
const (
	// DirName holds everything rig writes into a project.
	DirName = ".rig"
	// StateFile is the stack's description, under DirName.
	StateFile = "state.json"
	// LogsDir holds one log file per app, under DirName.
	LogsDir = "logs"
)

// Stack lifecycle states, as written to the state file.
const (
	StatusStarting = "starting"
	StatusReady    = "ready"
)

// ErrNotRunning is returned when there is no stack to act on.
var ErrNotRunning = errors.New("no rig running")

// AppState describes one running app.
type AppState struct {
	URL  string `json:"url"`
	Port int    `json:"port"`
	PID  int    `json:"pid,omitempty"`
	Log  string `json:"log"`
}

// ServiceState describes one running service.
type ServiceState struct {
	URL         string `json:"url"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Image       string `json:"image"`
	ContainerID string `json:"container_id,omitempty"`
}

// State is the stack as written to .rig/state.json. It is the one interface
// between rig and whatever drives the stack: Playwright, Dusk, pytest, an
// agent, or `rig down` in another shell.
type State struct {
	// PID is the rig process that owns the stack. `rig down` signals it.
	PID       int       `json:"pid"`
	Status    string    `json:"status"`
	Root      string    `json:"root"`
	StartedAt time.Time `json:"started_at"`

	// URL is the exposed app's URL: the stack's result.
	URL      string                  `json:"url,omitempty"`
	Expose   string                  `json:"expose"`
	Apps     map[string]AppState     `json:"apps"`
	Services map[string]ServiceState `json:"services"`
	// Env holds the RIG_* variables `rig run` passes to its command.
	Env map[string]string `json:"env,omitempty"`
}

// StatePath returns where the state file for a project root lives.
func StatePath(root string) string {
	return filepath.Join(root, DirName, StateFile)
}

// ReadState reads the state file for a project root. A missing file is
// ErrNotRunning.
func ReadState(root string) (*State, error) {
	raw, err := os.ReadFile(StatePath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w in %s", ErrNotRunning, root)
	}

	if err != nil {
		return nil, fmt.Errorf("reading state: %w", err)
	}

	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", StatePath(root), err)
	}

	return &st, nil
}

// Alive reports whether the rig process that wrote the state still exists.
func (s *State) Alive() bool {
	return processAlive(s.PID)
}

// writeState replaces the state file atomically, so a reader polling for it
// never sees half a file.
func writeState(root string, st *State) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding state: %w", err)
	}

	path := StatePath(root)

	tmp, err := os.CreateTemp(filepath.Dir(path), StateFile+".*")
	if err != nil {
		return fmt.Errorf("writing state: %w", err)
	}

	// The temp file is cleaned up on a best-effort basis: the write error is
	// the one worth reporting.
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())

		return fmt.Errorf("writing state: %w", err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("writing state: %w", err)
	}

	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing state: %w", err)
	}

	return nil
}

// RemoveState deletes the state file. A missing one is not an error.
func RemoveState(root string) error {
	if err := os.Remove(StatePath(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing state: %w", err)
	}

	return nil
}

// prepareDir creates .rig and its logs directory. A .gitignore inside it
// ignores everything, so a project never has to add .rig to its own.
func prepareDir(root string) error {
	dir := filepath.Join(root, DirName)

	if err := os.MkdirAll(filepath.Join(dir, LogsDir), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}

	ignore := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(ignore); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(ignore, []byte("*\n"), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", ignore, err)
		}
	}

	return nil
}
