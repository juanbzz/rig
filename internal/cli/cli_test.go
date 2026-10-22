package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/rs/zerolog"

	"github.com/juanbzz/rig/internal/config"
	"github.com/juanbzz/rig/internal/stack"
)

// _helperEnv makes the test binary serve HTTP instead of running tests, so
// the app in these stacks needs nothing installed on the machine.
const _helperEnv = "RIG_TEST_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(_helperEnv) == "serve" {
		srv := &http.Server{Addr: "127.0.0.1:" + os.Getenv("PORT"), ReadHeaderTimeout: time.Second}
		fmt.Fprintln(os.Stderr, srv.ListenAndServe())
		os.Exit(1)
	}

	os.Exit(m.Run())
}

type fakeEngine struct{}

func (fakeEngine) Start(context.Context, string, config.Service, map[string]string) (stack.Container, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	return &fakeContainer{l: l}, nil
}

type fakeContainer struct{ l net.Listener }

func (c *fakeContainer) ID() string   { return "fake" }
func (c *fakeContainer) Host() string { return "127.0.0.1" }
func (c *fakeContainer) Port() int {
	addr, ok := c.l.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}

	return addr.Port
}
func (c *fakeContainer) Terminate(context.Context) error { return c.l.Close() }

// project writes a .rig.toml with one service and one app and returns its
// root.
func project(t *testing.T) string {
	t.Helper()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	toml := fmt.Sprintf(`
expose = "web"
[services.db]
preset = "postgres"
[apps.web]
run = "%s=serve %s"
env = { PORT = "{{ port }}" }
`, _helperEnv, os.Args[0])

	if err := os.WriteFile(filepath.Join(root, config.FileName), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	return root
}

func newCLI() *CLI {
	return New(config.Env{LogLevel: "error"}).
		WithEngine(func(zerolog.Logger) stack.Engine { return fakeEngine{} })
}

// execute runs rig with args and returns its stdout, stderr, and error.
func execute(ctx context.Context, args ...string) (out, errOut string, err error) {
	var stdout, stderr syncBuffer

	root := newCLI().RootCommand()
	root.SetArgs(args)
	root.SetOut(&stdout)
	root.SetErr(&stderr)

	err = root.ExecuteContext(ctx)

	return stdout.String(), stderr.String(), err
}

func TestRun_PassesEnvAndExitCode(t *testing.T) {
	i := is.New(t)

	root := project(t)

	stdout, stderr, err := execute(context.Background(),
		"--config", filepath.Join(root, config.FileName),
		"run", "--", "sh", "-c", `echo "url=$RIG_URL db=$RIG_DB_URL"; test -f "$RIG_STATE"; exit 3`)

	var exit *CommandExitError
	i.True(errors.As(err, &exit))
	i.Equal(exit.Code, 3)
	i.Equal(ExitCode(err), 3)
	i.True(Silent(err))

	i.True(strings.HasPrefix(stdout, "url=http://localhost:"))
	i.True(strings.Contains(stdout, "db=postgres://rig:rig@127.0.0.1:"))
	i.Equal(stderr, "") // only the command speaks

	// The stack is gone afterwards.
	_, err = stack.ReadState(root)
	i.True(errors.Is(err, stack.ErrNotRunning))
}

func TestRun_Success(t *testing.T) {
	i := is.New(t)

	root := project(t)

	_, _, err := execute(context.Background(),
		"--config", filepath.Join(root, config.FileName), "run", "true")
	i.NoErr(err)
}

func TestRun_NeedsACommand(t *testing.T) {
	i := is.New(t)

	_, _, err := execute(context.Background(), "run")
	i.True(err != nil)
}

func TestUp_PrintsURLAndStopsOnCancel(t *testing.T) {
	i := is.New(t)

	root := project(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var stdout, stderr syncBuffer

	cmd := newCLI().RootCommand()
	cmd.SetArgs([]string{"--config", filepath.Join(root, config.FileName), "up"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()

	deadline := time.Now().Add(10 * time.Second)
	for stdout.String() == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}

	url := strings.TrimSpace(stdout.String())
	i.True(strings.HasPrefix(url, "http://localhost:"))

	st, err := stack.ReadState(root)
	i.NoErr(err)
	i.Equal(st.URL, url)
	i.Equal(st.Status, stack.StatusReady)

	cancel()

	select {
	case err := <-done:
		i.NoErr(err) // being stopped is how up ends
	case <-time.After(15 * time.Second):
		t.Fatal("up did not stop")
	}

	_, err = stack.ReadState(root)
	i.True(errors.Is(err, stack.ErrNotRunning))
	i.Equal(stderr.String(), "") // the URL on stdout is all up says
}

func TestUp_Verbose(t *testing.T) {
	i := is.New(t)

	root := project(t)

	_, stderr, err := execute(context.Background(),
		"--config", filepath.Join(root, config.FileName), "--verbose", "run", "true")
	i.NoErr(err)

	i.True(strings.Contains(stderr, "→ service db"))
	i.True(strings.Contains(stderr, "✓ ready in"))
	i.True(strings.Contains(stderr, "✓ stopped in"))
}

func TestDown_NothingRunning(t *testing.T) {
	i := is.New(t)

	root := project(t)

	_, stderr, err := execute(context.Background(), "--config", filepath.Join(root, config.FileName), "down")
	i.NoErr(err)
	i.Equal(stderr, "")

	_, stderr, err = execute(context.Background(), "--config", filepath.Join(root, config.FileName), "down", "--verbose")
	i.NoErr(err)
	i.True(strings.Contains(stderr, "no rig running"))
}

func TestDown_RemovesStaleState(t *testing.T) {
	i := is.New(t)

	root := project(t)
	i.NoErr(os.MkdirAll(filepath.Join(root, stack.DirName), 0o755))

	// A pid no process has: the rig that wrote it died without cleaning up.
	i.NoErr(os.WriteFile(stack.StatePath(root), []byte(`{"pid": 2147483646, "status": "ready"}`), 0o644))

	_, stderr, err := execute(context.Background(), "--config", filepath.Join(root, config.FileName), "down", "-v")
	i.NoErr(err)
	i.True(strings.Contains(stderr, "removed state left by pid 2147483646"))

	_, err = os.Stat(stack.StatePath(root))
	i.True(errors.Is(err, os.ErrNotExist))
}

func TestExitCode(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil", err: nil, want: 0},
		{name: "failure", err: errors.New("boom"), want: ExitError},
		{name: "usage", err: usageErrorf("bad"), want: ExitUsage},
		{name: "invalid config", err: fmt.Errorf("x: %w", config.ErrInvalid), want: ExitUsage},
		{name: "no config", err: config.ErrNotFound, want: ExitUsage},
		{name: "interrupted", err: fmt.Errorf("x: %w", context.Canceled), want: ExitInterrupted},
		{name: "command", err: &CommandExitError{Code: 42}, want: 42},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			is.New(t).Equal(ExitCode(tt.err), tt.want)
		})
	}
}

func TestSilent_NotWhenTeardownFailed(t *testing.T) {
	i := is.New(t)

	i.True(Silent(&CommandExitError{Code: 1}))
	i.True(!Silent(errors.Join(&CommandExitError{Code: 1}, errors.New("stopping service db"))))
	i.True(!Silent(errors.New("boom")))
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.b.String()
}

// TestSpinner_OnlyOnATerminal pins down when there is no spinner: its escape
// codes must never reach a pipe, a file, or a test buffer, and --verbose
// prints progress lines instead.
func TestSpinner_OnlyOnATerminal(t *testing.T) {
	file, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	tests := []struct {
		name    string
		stderr  io.Writer
		verbose bool
	}{
		{name: "verbose", stderr: os.Stderr, verbose: true},
		{name: "not a file", stderr: &bytes.Buffer{}},
		{name: "a file, not a terminal", stderr: file},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := is.New(t)

			c := newCLI()
			c.v.Set("verbose", tt.verbose)

			i.True(c.spinner(tt.stderr) == nil)
		})
	}
}

func TestVersion_StampWins(t *testing.T) {
	i := is.New(t)

	orig := _version
	t.Cleanup(func() { _version = orig })

	_version = "v1.2.3"
	i.Equal(version(), "v1.2.3")
}
