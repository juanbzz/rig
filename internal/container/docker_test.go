package container

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
)

// dockerConfig writes a Docker CLI config dir with one context selected.
func dockerConfig(t *testing.T, current, host string) string {
	t.Helper()
	i := is.New(t)

	dir := t.TempDir()
	i.NoErr(os.WriteFile(filepath.Join(dir, "config.json"),
		[]byte(`{"currentContext": "`+current+`"}`), 0o644))

	sum := sha256.Sum256([]byte(current))
	meta := filepath.Join(dir, "contexts", "meta", hex.EncodeToString(sum[:]))
	i.NoErr(os.MkdirAll(meta, 0o755))
	i.NoErr(os.WriteFile(filepath.Join(meta, "meta.json"),
		[]byte(`{"Name": "`+current+`", "Endpoints": {"docker": {"Host": "`+host+`"}}}`), 0o644))

	return dir
}

func TestConfigureDocker_FromContext(t *testing.T) {
	i := is.New(t)

	t.Setenv(_envDockerHost, "")
	t.Setenv(_envDockerContext, "")
	t.Setenv(_envSocketOverride, "")
	t.Setenv(_envDockerConfig, dockerConfig(t, "colima", "unix:///Users/me/.colima/default/docker.sock"))

	set, err := configureDocker()
	i.NoErr(err)
	i.Equal(set[_envDockerHost], "unix:///Users/me/.colima/default/docker.sock")
	i.Equal(set[_envSocketOverride], _daemonSocket)
	i.Equal(os.Getenv(_envDockerHost), "unix:///Users/me/.colima/default/docker.sock")
}

func TestConfigureDocker_RespectsUserSettings(t *testing.T) {
	i := is.New(t)

	t.Setenv(_envDockerHost, "tcp://10.0.0.5:2375")
	t.Setenv(_envSocketOverride, "")
	t.Setenv(_envDockerConfig, dockerConfig(t, "colima", "unix:///elsewhere.sock"))

	set, err := configureDocker()
	i.NoErr(err)
	i.Equal(len(set), 0) // a TCP host needs no socket mount, and DOCKER_HOST is the user's
}

func TestConfigureDocker_DefaultContext(t *testing.T) {
	i := is.New(t)

	t.Setenv(_envDockerHost, "")
	t.Setenv(_envDockerContext, "")
	t.Setenv(_envSocketOverride, "")
	t.Setenv(_envDockerConfig, dockerConfig(t, "default", "unix:///var/run/docker.sock"))

	set, err := configureDocker()
	i.NoErr(err)
	i.Equal(len(set), 0)
}

func TestConfigureDocker_ContextFromEnv(t *testing.T) {
	i := is.New(t)

	dir := dockerConfig(t, "orbstack", "unix:///Users/me/.orbstack/run/docker.sock")
	i.NoErr(os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"currentContext": "default"}`), 0o644))

	t.Setenv(_envDockerHost, "")
	t.Setenv(_envDockerContext, "orbstack")
	t.Setenv(_envSocketOverride, "")
	t.Setenv(_envDockerConfig, dir)

	set, err := configureDocker()
	i.NoErr(err)
	i.Equal(set[_envDockerHost], "unix:///Users/me/.orbstack/run/docker.sock")
}
