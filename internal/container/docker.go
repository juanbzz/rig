package container

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Environment variables that steer the Docker client and testcontainers.
const (
	_envDockerHost     = "DOCKER_HOST"
	_envDockerContext  = "DOCKER_CONTEXT"
	_envDockerConfig   = "DOCKER_CONFIG"
	_envSocketOverride = "TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"
)

// _daemonSocket is where the daemon's socket lives inside the machine the
// daemon runs on, which for colima, OrbStack, and Docker Desktop is a VM.
const _daemonSocket = "/var/run/docker.sock"

// configureDocker fills in what testcontainers cannot find for itself on a
// machine whose Docker runs in a VM, and returns what it changed for logging.
//
// testcontainers reads DOCKER_HOST but not `docker context`, so with colima
// or OrbStack selected through a context it finds no daemon at all. Once it
// does, it mounts the client-side socket path into its Ryuk reaper, a path
// that exists on the Mac but not in the VM, and the reaper fails to start.
// The daemon's own socket path inside the VM is the right mount.
//
// Variables the user set are never overridden.
//
// This changes the process environment, the one place testcontainers reads
// from, so steps and apps inherit the values too. That is harmless, since
// they describe the same daemon, and it means a step that runs `docker`
// talks to the daemon rig used.
func configureDocker() (map[string]string, error) {
	set := map[string]string{}

	if os.Getenv(_envDockerHost) == "" {
		host, err := contextHost()
		if err != nil {
			return nil, err
		}

		if host != "" {
			set[_envDockerHost] = host
		}
	}

	host := os.Getenv(_envDockerHost)
	if v, ok := set[_envDockerHost]; ok {
		host = v
	}

	if os.Getenv(_envSocketOverride) == "" && strings.HasPrefix(host, "unix://") &&
		strings.TrimPrefix(host, "unix://") != _daemonSocket {
		set[_envSocketOverride] = _daemonSocket
	}

	for k, v := range set {
		if err := os.Setenv(k, v); err != nil {
			return nil, fmt.Errorf("setting %s: %w", k, err)
		}
	}

	return set, nil
}

// contextHost returns the daemon address of the selected `docker context`, or
// "" when the default context is in use.
func contextHost() (string, error) {
	dir := os.Getenv(_envDockerConfig)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil //nolint:nilerr // no home, no contexts: use the defaults
		}

		dir = filepath.Join(home, ".docker")
	}

	name := os.Getenv(_envDockerContext)
	if name == "" {
		var err error
		if name, err = currentContext(dir); err != nil {
			return "", err
		}
	}

	if name == "" || name == "default" {
		return "", nil
	}

	// The Docker CLI stores each context under the SHA-256 of its name.
	sum := sha256.Sum256([]byte(name))
	path := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]), "meta.json")

	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading docker context %q: %w", name, err)
	}

	var meta struct {
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}

	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", fmt.Errorf("parsing docker context %q: %w", name, err)
	}

	return meta.Endpoints["docker"].Host, nil
}

// currentContext reads the context the Docker CLI has selected.
func currentContext(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("reading docker config: %w", err)
	}

	var cfg struct {
		CurrentContext string `json:"currentContext"`
	}

	if err := json.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("parsing docker config: %w", err)
	}

	return cfg.CurrentContext, nil
}
