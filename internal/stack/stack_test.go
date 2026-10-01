package stack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/matryer/is"
	"github.com/rs/zerolog"

	"github.com/j0lvera/rig/internal/config"
)

// _helperEnv makes the test binary act as an app instead of running tests,
// so apps in these tests are real processes serving real HTTP without
// depending on anything installed on the machine.
const _helperEnv = "RIG_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(_helperEnv) {
	case "":
		os.Exit(m.Run())
	case "serve":
		serve()
	case "crash":
		fmt.Println("booting")
		fmt.Fprintln(os.Stderr, "fatal: no database")
		os.Exit(3)
	}
}

// serve answers /health and echoes its environment at /env until killed.
func serve() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/env", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, os.Getenv(r.URL.Query().Get("key")))
	})

	fmt.Println("listening on", os.Getenv("PORT"))

	srv := &http.Server{Addr: "127.0.0.1:" + os.Getenv("PORT"), Handler: mux, ReadHeaderTimeout: time.Second}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// helper returns a run command that starts the test binary in a mode.
func helper(mode string) string {
	return fmt.Sprintf("%s=%s %s", _helperEnv, mode, os.Args[0])
}

// fakeEngine hands out containers that are plain TCP listeners, so the
// orchestration runs without Docker.
type fakeEngine struct {
	mu         sync.Mutex
	started    []string
	terminated []string
	fail       map[string]error
}

func (f *fakeEngine) Start(_ context.Context, name string, _ config.Service, labels map[string]string) (Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.fail[name]; err != nil {
		return nil, err
	}

	if labels["dev.rig.service"] != name {
		return nil, fmt.Errorf("missing service label on %s", name)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}

	f.started = append(f.started, name)

	return &fakeContainer{engine: f, name: name, l: l}, nil
}

func (f *fakeEngine) terminatedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.terminated...)
}

type fakeContainer struct {
	engine *fakeEngine
	name   string
	l      net.Listener
}

func (c *fakeContainer) ID() string   { return "fake-" + c.name }
func (c *fakeContainer) Host() string { return "127.0.0.1" }
func (c *fakeContainer) Port() int {
	addr, ok := c.l.Addr().(*net.TCPAddr)
	if !ok {
		return 0
	}

	return addr.Port
}

func (c *fakeContainer) Terminate(context.Context) error {
	c.engine.mu.Lock()
	c.engine.terminated = append(c.engine.terminated, c.name)
	c.engine.mu.Unlock()

	return c.l.Close()
}

// parse loads a config rooted in a fresh temp dir.
func parse(t *testing.T, toml string) *config.Config {
	t.Helper()

	cfg, err := config.Parse([]byte(toml))
	if err != nil {
		t.Fatalf("parsing config: %v", err)
	}

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	cfg.Root = root
	cfg.Path = filepath.Join(root, config.FileName)

	return cfg
}

func options(engine Engine, out *bytes.Buffer) Options {
	return Options{Engine: engine, Out: &lockedBuffer{b: out}, Logger: zerolog.Nop()}
}

// lockedBuffer lets the test read output the stack writes from goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  *bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.b.String()
}

func get(t *testing.T, url string) string {
	t.Helper()

	resp, err := http.Get(url) //nolint:noctx // test helper
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	var b bytes.Buffer
	if _, err := b.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}

	return b.String()
}

func TestUp_FullStack(t *testing.T) {
	i := is.New(t)

	cfg := parse(t, `
expose = "web"

[services.db]
preset = "postgres"

[[steps]]
name = "migrate"
run  = "echo \"$GOOSE_DBSTRING\" > migrated.txt"
env  = { GOOSE_DBSTRING = "{{ services.db.url }}" }

[apps.api]
build  = "echo built > {{ tmp }}/api.txt"
run    = "`+helper("serve")+`"
health = "/health"
env    = { PORT = "{{ port }}", DATABASE_URL = "{{ services.db.url }}", ORIGINS = "{{ apps.web.url }}" }

[apps.web]
run        = "`+helper("serve")+`"
env        = { PORT = "{{ port }}", API = "{{ apps.api.url }}", DB_PASSWORD = "{{ services.db.password }}" }
depends_on = ["api"]
`)

	engine := &fakeEngine{}
	var out bytes.Buffer

	st, err := Up(context.Background(), cfg, options(engine, &out))
	i.NoErr(err)

	state := st.State()
	i.Equal(state.Status, StatusReady)
	i.Equal(state.URL, state.Apps["web"].URL)

	db := state.Services["db"]
	i.Equal(db.URL, fmt.Sprintf("postgres://rig:rig@127.0.0.1:%d/rig?sslmode=disable", db.Port))

	// The step ran in the root, after the service, with its URL.
	migrated, err := os.ReadFile(filepath.Join(cfg.Root, "migrated.txt"))
	i.NoErr(err)
	i.Equal(strings.TrimSpace(string(migrated)), db.URL)

	// Each app sees the others' URLs, in both directions.
	api, web := state.Apps["api"], state.Apps["web"]
	i.Equal(get(t, api.URL+"/env?key=ORIGINS"), web.URL)
	i.Equal(get(t, web.URL+"/env?key=API"), api.URL)
	i.Equal(get(t, api.URL+"/env?key=DATABASE_URL"), db.URL)
	i.Equal(get(t, web.URL+"/env?key=DB_PASSWORD"), "rig")

	// The state file is what other processes read.
	onDisk, err := ReadState(cfg.Root)
	i.NoErr(err)
	i.Equal(onDisk.URL, state.URL)
	i.Equal(onDisk.PID, os.Getpid())
	i.Equal(onDisk.Env["RIG_URL"], web.URL)
	i.Equal(onDisk.Env["RIG_API_URL"], api.URL)
	i.Equal(onDisk.Env["RIG_DB_URL"], db.URL)

	i.True(strings.Contains(strings.Join(st.Env(), "\n"), "RIG_WEB_URL="+web.URL))

	log, err := os.ReadFile(api.Log)
	i.NoErr(err)
	i.True(strings.Contains(string(log), "listening on "+strconv.Itoa(api.Port)))

	i.NoErr(st.Down(context.Background()))

	i.Equal(engine.terminatedNames(), []string{"db"})

	_, err = ReadState(cfg.Root)
	i.True(errors.Is(err, ErrNotRunning))

	_, err = os.Stat(state.Apps["api"].Log)
	i.NoErr(err) // logs outlive the stack, for reading after a failure

	_, err = net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(api.Port)))
	i.True(err != nil) // nothing is listening any more

	// Down is idempotent.
	i.NoErr(st.Down(context.Background()))
}

func TestUp_AppCrashesBeforeHealthy(t *testing.T) {
	i := is.New(t)

	cfg := parse(t, `
expose = "web"
[services.db]
preset = "postgres"
[apps.web]
run = "`+helper("crash")+`"
`)

	engine := &fakeEngine{}
	var out bytes.Buffer

	began := time.Now()
	_, err := Up(context.Background(), cfg, options(engine, &out))
	i.True(err != nil)

	msg := err.Error()
	i.True(strings.Contains(msg, "app web exited with status 3 before it was healthy"))
	i.True(strings.Contains(msg, "fatal: no database")) // the log tail
	i.True(time.Since(began) < 5*time.Second)           // not the full timeout

	// Whatever started is torn down.
	i.Equal(engine.terminatedNames(), []string{"db"})

	_, err = ReadState(cfg.Root)
	i.True(errors.Is(err, ErrNotRunning))
}

func TestUp_HealthTimeout(t *testing.T) {
	i := is.New(t)

	cfg := parse(t, `
expose = "web"
[apps.web]
run     = "sleep 30"
health  = "/health"
timeout = "500ms"
`)

	var out bytes.Buffer

	_, err := Up(context.Background(), cfg, options(&fakeEngine{}, &out))
	i.True(errors.Is(err, ErrUnhealthy))
	i.True(strings.Contains(err.Error(), "app web: not healthy after 500ms"))
}

func TestUp_StepFails(t *testing.T) {
	i := is.New(t)

	cfg := parse(t, `
expose = "web"
[services.db]
preset = "postgres"
[[steps]]
name = "seed"
run  = "echo seeded"
[[steps]]
name = "migrate"
run  = "echo boom; exit 4"
[apps.web]
run = "`+helper("serve")+`"
env = { PORT = "{{ port }}" }
`)

	engine := &fakeEngine{}
	var out bytes.Buffer

	_, err := Up(context.Background(), cfg, options(engine, &out))
	i.True(errors.Is(err, ErrFailed))

	// Quiet by default, so the error carries the step's own output, and only
	// that step's: the earlier one's line is not quoted.
	msg := err.Error()
	i.True(strings.HasPrefix(msg, "step migrate failed: exited with status 4\n"))
	i.True(strings.Contains(msg, "last lines of .rig/logs/setup.log:\n    boom"))
	i.True(!strings.Contains(msg, "seeded"))
	i.Equal(out.String(), "")

	log, err := os.ReadFile(filepath.Join(cfg.Root, DirName, LogsDir, SetupLog))
	i.NoErr(err)
	i.True(strings.Contains(string(log), "→ step seed\nseeded\n→ step migrate\nboom\n"))

	i.Equal(engine.terminatedNames(), []string{"db"})
}

func TestUp_QuietUnlessVerbose(t *testing.T) {
	toml := `
expose = "web"
[[steps]]
name = "migrate"
run  = "echo migrating"
[apps.web]
run = "` + helper("serve") + `"
env = { PORT = "{{ port }}" }
`

	t.Run("quiet", func(t *testing.T) {
		i := is.New(t)

		var out bytes.Buffer

		st, err := Up(context.Background(), parse(t, toml), options(&fakeEngine{}, &out))
		i.NoErr(err)
		i.NoErr(st.Down(context.Background()))
		i.Equal(out.String(), "")
	})

	t.Run("verbose", func(t *testing.T) {
		i := is.New(t)

		var out bytes.Buffer

		opts := options(&fakeEngine{}, &out)
		opts.Verbose = true

		st, err := Up(context.Background(), parse(t, toml), opts)
		i.NoErr(err)
		i.NoErr(st.Down(context.Background()))

		got := out.String()
		i.True(strings.Contains(got, "→ step migrate\nmigrating\n"))
		i.True(strings.Contains(got, "web │ listening on"))
		i.True(strings.Contains(got, "✓ ready in"))
		i.True(strings.Contains(got, "✓ stopped in"))
	})
}

func TestUp_ServiceFails(t *testing.T) {
	i := is.New(t)

	cfg := parse(t, `
expose = "web"
[services.a]
preset = "postgres"
[services.b]
preset = "mysql"
[apps.web]
run = "x"
`)

	engine := &fakeEngine{fail: map[string]error{"b": errors.New("pull failed")}}
	var out bytes.Buffer

	_, err := Up(context.Background(), cfg, options(engine, &out))
	i.True(err != nil)
	i.Equal(err.Error(), "service b: pull failed")

	// Any service that did start is torn down.
	i.Equal(engine.terminatedNames(), engine.started)
}

func TestUp_Interrupted(t *testing.T) {
	i := is.New(t)

	cfg := parse(t, `
expose = "web"
[[steps]]
run = "sleep 30"
[apps.web]
run = "x"
`)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	var out bytes.Buffer

	began := time.Now()
	_, err := Up(ctx, cfg, options(&fakeEngine{}, &out))
	i.True(errors.Is(err, context.DeadlineExceeded))
	i.True(time.Since(began) < 5*time.Second) // the step was stopped, not waited out
}

func TestUp_AlreadyRunning(t *testing.T) {
	i := is.New(t)

	cfg := parse(t, `
expose = "web"
[apps.web]
run = "x"
`)

	i.NoErr(prepareDir(cfg.Root))

	// A live process that is not this one owns the stack. The parent of the
	// test binary is as good as any.
	i.NoErr(writeState(cfg.Root, &State{PID: os.Getppid(), Status: StatusReady}))

	var out bytes.Buffer

	_, err := Up(context.Background(), cfg, options(&fakeEngine{}, &out))
	i.True(errors.Is(err, ErrAlreadyRunning))

	// The other rig's state is left alone.
	st, err := ReadState(cfg.Root)
	i.NoErr(err)
	i.Equal(st.PID, os.Getppid())
}

func TestUp_ReplacesStaleState(t *testing.T) {
	i := is.New(t)

	cfg := parse(t, `
expose = "web"
[apps.web]
run = "`+helper("serve")+`"
env = { PORT = "{{ port }}" }
`)

	i.NoErr(prepareDir(cfg.Root))
	i.NoErr(writeState(cfg.Root, &State{PID: deadPID(t), Status: StatusReady}))

	var out bytes.Buffer

	st, err := Up(context.Background(), cfg, options(&fakeEngine{}, &out))
	i.NoErr(err)
	i.NoErr(st.Down(context.Background()))
}

func TestStack_Crashed(t *testing.T) {
	i := is.New(t)

	cfg := parse(t, `
expose = "web"
[apps.web]
run = "`+helper("serve")+`"
env = { PORT = "{{ port }}" }
`)

	var out bytes.Buffer

	st, err := Up(context.Background(), cfg, options(&fakeEngine{}, &out))
	i.NoErr(err)

	defer func() { i.NoErr(st.Down(context.Background())) }()

	pid := st.State().Apps["web"].PID
	i.NoErr(signalGroup(pid, 9))

	select {
	case err := <-st.Crashed():
		i.True(strings.Contains(err.Error(), "app web was killed by signal 9 (killed)"))
	case <-time.After(5 * time.Second):
		t.Fatal("crash not reported")
	}
}

func TestUp_GitignoresItsDir(t *testing.T) {
	i := is.New(t)

	cfg := parse(t, `
expose = "web"
[apps.web]
run = "x"
timeout = "100ms"
`)

	var out bytes.Buffer

	_, _ = Up(context.Background(), cfg, options(&fakeEngine{}, &out))

	ignore, err := os.ReadFile(filepath.Join(cfg.Root, DirName, ".gitignore"))
	i.NoErr(err)
	i.Equal(string(ignore), "*\n")
}

// deadPID returns the pid of a process that has exited.
func deadPID(t *testing.T) int {
	t.Helper()

	p, err := startProc(procSpec{command: "true"})
	if err != nil {
		t.Fatal(err)
	}

	<-p.Done()

	return p.PID()
}

// recorder is a Reporter that keeps every status it is given.
type recorder struct {
	mu       sync.Mutex
	statuses []string
}

func (r *recorder) StepStarted(desc string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.statuses = append(r.statuses, desc)
}

func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.statuses...)
}

func TestUp_ReportsEachPhase(t *testing.T) {
	i := is.New(t)

	cfg := parse(t, `
expose = "web"
[services.db]
preset = "postgres"
[services.s3]
image = "minio/minio"
port  = 9000
[[steps]]
name = "migrate"
run  = "true"
[apps.api]
build = "true"
run   = "`+helper("serve")+`"
env   = { PORT = "{{ port }}" }
[apps.web]
run        = "`+helper("serve")+`"
env        = { PORT = "{{ port }}" }
depends_on = ["api"]
`)

	rec := &recorder{}
	var out bytes.Buffer

	opts := options(&fakeEngine{}, &out)
	opts.Reporter = rec

	st, err := Up(context.Background(), cfg, opts)
	i.NoErr(err)
	i.NoErr(st.Down(context.Background()))

	got := rec.all()

	// Parallel phases open with everything they wait on, then narrow.
	i.Equal(got[0], "starting db, s3")
	i.True(slices.Contains(got, "step migrate"))
	i.True(slices.Contains(got, "build api"))
	i.True(slices.Contains(got, "starting api, web"))
	i.True(slices.Contains(got, "starting web")) // web outlasts api, which it waits on
	i.Equal(got[len(got)-1], "stopping")

	// Phases come in order: services, steps, builds, apps.
	i.True(slices.Index(got, "step migrate") < slices.Index(got, "build api"))
	i.True(slices.Index(got, "build api") < slices.Index(got, "starting api, web"))

	i.Equal(out.String(), "") // the reporter is for a quiet run
}
