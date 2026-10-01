// Package stack turns a config into a running, disposable stack and tears it
// down again.
//
// A stack comes up in a fixed order. Every app's port is assigned first, so
// any template can refer to any app regardless of start order. Services start
// next, in parallel, then steps run one at a time, then app builds, and then
// apps start, each one waiting only for the apps it depends on. The state
// file is written at the start and again once everything is healthy, so a
// reader always knows whether the stack is usable yet.
package stack

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/j0lvera/rig/internal/config"
	"github.com/j0lvera/rig/internal/tmpl"
)

var (
	// ErrAlreadyRunning is returned when another rig owns the stack for this
	// project.
	ErrAlreadyRunning = errors.New("rig is already running")
	// ErrFailed is returned when a step or an app build exits non-zero.
	ErrFailed = errors.New("failed")
)

// _appHost is the host every app is reached on. It is a name rather than
// 127.0.0.1 because dev servers such as Vite bind to whichever of ::1 and
// 127.0.0.1 "localhost" resolves to first, and a name reaches either.
const _appHost = "localhost"

// _stopGrace is how long a process group gets between SIGTERM and SIGKILL.
const _stopGrace = 10 * time.Second

// _terminateTimeout bounds stopping every container.
const _terminateTimeout = 30 * time.Second

// _logTail is how many lines of an app's log an error message quotes.
const _logTail = 20

// Container is a running service.
type Container interface {
	ID() string
	// Host and Port are where the service is reached from this machine.
	Host() string
	Port() int
	Terminate(ctx context.Context) error
}

// Engine starts services. It is an interface so the orchestration can be
// tested without Docker.
type Engine interface {
	Start(ctx context.Context, name string, svc config.Service, labels map[string]string) (Container, error)
}

// Options configures a stack.
type Options struct {
	Engine Engine
	// Out receives progress lines and step output.
	Out io.Writer
	// StreamLogs also copies app output to Out. It always goes to
	// .rig/logs/<app>.log.
	StreamLogs bool
	Logger     zerolog.Logger
}

// Stack is a running stack. Down must be called once it is no longer needed.
type Stack struct {
	cfg    *config.Config
	opts   Options
	out    *syncWriter
	tmp    string
	scope  tmpl.Scope
	ports  map[string]int
	state  State
	start  time.Time
	logger zerolog.Logger

	mu         sync.Mutex
	containers map[string]Container
	apps       map[string]*proc
	logs       []*os.File

	streams []*prefixWriter

	stopping chan struct{}
	// crashed is buffered for every app: each watcher sends at most once, so
	// none can block, even when nothing reads because the stack is stopping.
	crashed  chan error
	watchers sync.WaitGroup
	downOnce sync.Once
	downErr  error
}

// Up starts the stack described by cfg and returns once every app is healthy.
// On failure it tears down whatever it started before returning.
func Up(ctx context.Context, cfg *config.Config, opts Options) (*Stack, error) {
	out := opts.Out
	if out == nil {
		out = io.Discard
	}

	s := &Stack{
		cfg:        cfg,
		opts:       opts,
		out:        &syncWriter{w: out},
		scope:      tmpl.Scope{},
		ports:      map[string]int{},
		start:      time.Now(),
		logger:     opts.Logger,
		containers: map[string]Container{},
		apps:       map[string]*proc{},
		stopping:   make(chan struct{}),
		crashed:    make(chan error, len(cfg.Apps)),
		state: State{
			PID:       os.Getpid(),
			Status:    StatusStarting,
			Root:      cfg.Root,
			Expose:    cfg.Expose,
			Apps:      map[string]AppState{},
			Services:  map[string]ServiceState{},
			StartedAt: time.Now().UTC(),
		},
	}

	if err := s.claim(); err != nil {
		return nil, err
	}

	if err := s.up(ctx); err != nil {
		// Teardown must run even though ctx may be the reason for failing.
		downErr := s.Down(context.WithoutCancel(ctx))
		return nil, errors.Join(err, downErr)
	}

	return s, nil
}

// claim makes the stack this project's, refusing if a live rig already owns
// it, and records it as starting so `rig down` can reach it during startup.
func (s *Stack) claim() error {
	root := s.cfg.Root

	if err := prepareDir(root); err != nil {
		return err
	}

	prev, err := ReadState(root)

	switch {
	case errors.Is(err, ErrNotRunning):
	case err != nil:
		s.logger.Warn().Err(err).Msg("ignoring unreadable state file")
	case prev.Alive() && prev.PID != os.Getpid():
		return fmt.Errorf("%w in %s (pid %d); stop it with `rig down`", ErrAlreadyRunning, root, prev.PID)
	default:
		s.warnLeftovers(prev)
	}

	return writeState(root, &s.state)
}

// warnLeftovers reports app processes a rig that died without tearing down
// may have left behind. They are not killed: after a crash, the pid in the
// file may since have gone to an unrelated process.
func (s *Stack) warnLeftovers(prev *State) {
	for _, name := range slices.Sorted(maps.Keys(prev.Apps)) {
		app := prev.Apps[name]
		if app.PID > 0 && groupAlive(app.PID) {
			s.out.printf("! a previous rig (pid %d) did not stop cleanly; app %s may still be running "+
				"as process group %d on port %d\n", prev.PID, name, app.PID, app.Port)
		}
	}
}

func (s *Stack) up(ctx context.Context) error {
	tmp, err := os.MkdirTemp("", "rig-")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}

	s.tmp = tmp
	s.scope[config.VarTmp] = tmp
	s.scope[config.VarRoot] = s.cfg.Root

	if err := s.assignPorts(); err != nil {
		return err
	}

	if err := s.startServices(ctx); err != nil {
		return err
	}

	for i, step := range s.cfg.Steps {
		if err := s.runStep(ctx, i, step); err != nil {
			return err
		}
	}

	for _, name := range s.cfg.AppNames() {
		if err := s.buildApp(ctx, name); err != nil {
			return err
		}
	}

	if err := s.startApps(ctx); err != nil {
		return err
	}

	return s.ready()
}

// assignPorts gives every app a free port and puts its address in scope. This
// happens before anything starts, which is what lets two apps refer to each
// other: an API whose CORS origin is the web app's URL, and a web app that
// proxies to the API.
func (s *Stack) assignPorts() error {
	names := s.cfg.AppNames()

	ports, err := freePorts(len(names))
	if err != nil {
		return err
	}

	for i, name := range names {
		port := ports[i]
		url := "http://" + _appHost + ":" + strconv.Itoa(port)

		s.ports[name] = port
		s.scope[config.AppVar(name, config.FieldPort)] = strconv.Itoa(port)
		s.scope[config.AppVar(name, config.FieldHost)] = _appHost
		s.scope[config.AppVar(name, config.FieldURL)] = url

		s.state.Apps[name] = AppState{
			URL:  url,
			Port: port,
			Log:  filepath.Join(s.cfg.Root, DirName, LogsDir, name+".log"),
		}
	}

	return nil
}

// startServices starts every container in parallel. They depend on nothing
// but their images, and pulling and booting them is most of a cold start.
func (s *Stack) startServices(ctx context.Context) error {
	g, ctx := newGroup(ctx)

	for _, name := range s.cfg.ServiceNames() {
		g.Go(func() error { return s.startService(ctx, name) })
	}

	return g.Wait()
}

func (s *Stack) startService(ctx context.Context, name string) error {
	svc := s.cfg.Services[name]
	began := time.Now()

	s.out.printf("→ service %s  %s\n", name, svc.Image)

	labels := map[string]string{
		"dev.rig.root":    s.cfg.Root,
		"dev.rig.service": name,
	}

	c, err := s.opts.Engine.Start(ctx, name, svc, labels)
	if err != nil {
		return fmt.Errorf("service %s: %w", name, err)
	}

	s.mu.Lock()
	s.containers[name] = c
	s.mu.Unlock()

	local := tmpl.Scope{
		config.FieldHost: c.Host(),
		config.FieldPort: strconv.Itoa(c.Port()),
	}
	maps.Copy(local, svc.Vars)

	url, err := tmpl.Render(svc.URL, local)
	if err != nil {
		return fmt.Errorf("services.%s.url: %w", name, err)
	}

	s.mu.Lock()
	s.scope[config.ServiceVar(name, config.FieldURL)] = url
	s.scope[config.ServiceVar(name, config.FieldHost)] = c.Host()
	s.scope[config.ServiceVar(name, config.FieldPort)] = strconv.Itoa(c.Port())

	for k, v := range svc.Vars {
		s.scope[config.ServiceVar(name, k)] = v
	}

	s.state.Services[name] = ServiceState{
		URL:         url,
		Host:        c.Host(),
		Port:        c.Port(),
		Image:       svc.Image,
		ContainerID: c.ID(),
	}
	s.mu.Unlock()

	s.out.printf("✓ service %s  ready in %s\n", name, since(began))

	return nil
}

func (s *Stack) runStep(ctx context.Context, i int, step config.Step) error {
	where := fmt.Sprintf("steps[%d]", i)

	run, err := tmpl.Render(step.Run, s.scope)
	if err != nil {
		return fmt.Errorf("%s.run: %w", where, err)
	}

	env, err := tmpl.RenderMap(step.Env, s.scope)
	if err != nil {
		return fmt.Errorf("%s.env.%w", where, err)
	}

	return s.runOnce(ctx, "step "+step.Label(), run, s.cfg.Dir(step.Dir), env)
}

func (s *Stack) buildApp(ctx context.Context, name string) error {
	app := s.cfg.Apps[name]
	if app.Build == "" {
		return nil
	}

	build, err := tmpl.Render(app.Build, s.appScope(name))
	if err != nil {
		return fmt.Errorf("apps.%s.build: %w", name, err)
	}

	env, err := tmpl.RenderMap(app.Env, s.appScope(name))
	if err != nil {
		return fmt.Errorf("apps.%s.env.%w", name, err)
	}

	return s.runOnce(ctx, "build "+name, build, s.cfg.Dir(app.Dir), env)
}

// runOnce runs a command to completion with its output streamed: a migration
// or an asset build can take minutes, and a silent minute looks like a hang.
func (s *Stack) runOnce(ctx context.Context, label, command, dir string, env map[string]string) error {
	s.out.printf("→ %s\n", label)
	s.logger.Debug().Str("run", command).Str("dir", dir).Msg("command run")

	began := time.Now()

	p, err := startProc(procSpec{
		command:   command,
		dir:       dir,
		env:       environ(env),
		stdout:    s.out,
		stderr:    s.out,
		waitDelay: _stopGrace,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}

	select {
	case <-p.Done():
	case <-ctx.Done():
		if err := p.Stop(_stopGrace); err != nil {
			s.logger.Debug().Err(err).Str("command", label).Msg("unable to stop command")
		}

		return ctx.Err()
	}

	if err := p.Err(); err != nil {
		return fmt.Errorf("%s %w: %s", label, ErrFailed, exitStatus(err))
	}

	s.out.printf("✓ %s  done in %s\n", label, since(began))

	return nil
}

// startApps starts every app at once, except that an app waits for the apps
// in its depends_on to be healthy first.
func (s *Stack) startApps(ctx context.Context) error {
	healthy := make(map[string]chan struct{}, len(s.cfg.Apps))
	for name := range s.cfg.Apps {
		healthy[name] = make(chan struct{})
	}

	g, ctx := newGroup(ctx)

	for _, name := range s.cfg.AppNames() {
		g.Go(func() error {
			if err := s.startApp(ctx, name, healthy); err != nil {
				return err
			}

			close(healthy[name])

			return nil
		})
	}

	return g.Wait()
}

func (s *Stack) startApp(ctx context.Context, name string, healthy map[string]chan struct{}) error {
	app := s.cfg.Apps[name]

	for _, dep := range app.DependsOn {
		select {
		case <-healthy[dep]:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	scope := s.appScope(name)

	run, err := tmpl.Render(app.Run, scope)
	if err != nil {
		return fmt.Errorf("apps.%s.run: %w", name, err)
	}

	env, err := tmpl.RenderMap(app.Env, scope)
	if err != nil {
		return fmt.Errorf("apps.%s.env.%w", name, err)
	}

	s.mu.Lock()
	logPath, url := s.state.Apps[name].Log, s.state.Apps[name].URL
	s.mu.Unlock()

	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("app %s: creating log: %w", name, err)
	}

	s.mu.Lock()
	s.logs = append(s.logs, logFile)
	s.mu.Unlock()

	// Without streaming, the process writes straight to the file, so nothing
	// in rig sits between it and its output.
	var w io.Writer = logFile
	if s.opts.StreamLogs {
		stream := newPrefixWriter(s.out, name+" │ ")
		w = io.MultiWriter(logFile, stream)

		s.mu.Lock()
		s.streams = append(s.streams, stream)
		s.mu.Unlock()
	}

	began := time.Now()

	s.out.printf("→ app %s  %s\n", name, url)
	s.logger.Debug().Str("run", run).Str("dir", s.cfg.Dir(app.Dir)).Msg("app start")

	p, err := startProc(procSpec{
		command:   run,
		dir:       s.cfg.Dir(app.Dir),
		env:       environ(env),
		stdout:    w,
		stderr:    w,
		waitDelay: _stopGrace,
	})
	if err != nil {
		return fmt.Errorf("app %s: %w", name, err)
	}

	s.mu.Lock()
	s.apps[name] = p
	st := s.state.Apps[name]
	st.PID = p.PID()
	s.state.Apps[name] = st
	s.mu.Unlock()

	check := healthCheck{host: _appHost, port: s.ports[name], path: app.Health}

	err = waitHealthy(ctx, check, app.Timeout.Duration, p.Done())

	switch {
	case err == nil:
	case errors.Is(err, ErrExited):
		return fmt.Errorf("app %s %s before it was healthy%s", name, exitStatus(p.Err()), s.logExcerpt(logPath))
	case errors.Is(err, ErrUnhealthy):
		return fmt.Errorf("app %s: %w%s", name, err, s.logExcerpt(logPath))
	default:
		return err
	}

	s.out.printf("✓ app %s  healthy in %s\n", name, since(began))

	return nil
}

// appScope is the stack-wide scope plus the app's own {{ port }}.
func (s *Stack) appScope(name string) tmpl.Scope {
	return s.scope.With(tmpl.Scope{config.VarPort: strconv.Itoa(s.ports[name])})
}

// ready records the finished stack and starts watching for apps that die.
func (s *Stack) ready() error {
	s.state.Status = StatusReady
	s.state.URL = s.state.Apps[s.cfg.Expose].URL
	s.state.Env = s.env()

	if err := writeState(s.cfg.Root, &s.state); err != nil {
		return err
	}

	for name, p := range s.apps {
		s.watchers.Go(func() {
			select {
			case <-p.Done():
				select {
				case <-s.stopping:
				default:
					s.crashed <- fmt.Errorf("app %s %s%s", name, exitStatus(p.Err()),
						s.logExcerpt(s.state.Apps[name].Log))
				}
			case <-s.stopping:
			}
		})
	}

	s.out.printf("✓ ready in %s  %s\n", since(s.start), s.state.URL)

	return nil
}

// env is the set of RIG_* variables describing the stack.
func (s *Stack) env() map[string]string {
	env := map[string]string{
		"RIG_URL":   s.state.URL,
		"RIG_STATE": StatePath(s.cfg.Root),
	}

	for name, app := range s.state.Apps {
		env["RIG_"+config.EnvName(name)+"_URL"] = app.URL
	}

	for name, svc := range s.state.Services {
		env["RIG_"+config.EnvName(name)+"_URL"] = svc.URL
	}

	return env
}

// State returns a copy of the stack's state.
func (s *Stack) State() State {
	st := s.state
	st.Apps = maps.Clone(s.state.Apps)
	st.Services = maps.Clone(s.state.Services)
	st.Env = maps.Clone(s.state.Env)

	return st
}

// Env returns the RIG_* variables as KEY=value pairs, sorted.
func (s *Stack) Env() []string {
	pairs := make([]string, 0, len(s.state.Env))
	for _, k := range slices.Sorted(maps.Keys(s.state.Env)) {
		pairs = append(pairs, k+"="+s.state.Env[k])
	}

	return pairs
}

// Crashed delivers an error when an app exits while the stack is up.
func (s *Stack) Crashed() <-chan error {
	return s.crashed
}

// Down stops every app, then every container, and removes the state file.
// It is safe to call more than once.
func (s *Stack) Down(ctx context.Context) error {
	s.downOnce.Do(func() { s.downErr = s.down(ctx) })
	return s.downErr
}

func (s *Stack) down(ctx context.Context) error {
	close(s.stopping)

	began := time.Now()

	s.mu.Lock()
	apps := maps.Clone(s.apps)
	containers := maps.Clone(s.containers)
	s.mu.Unlock()

	if len(apps)+len(containers) > 0 {
		s.out.printf("→ stopping\n")
	}

	// Apps first: they hold connections to the services, and an app that
	// loses its database mid-shutdown only adds noise to the log.
	var wg sync.WaitGroup
	for name, p := range apps {
		wg.Go(func() {
			if err := p.Stop(_stopGrace); err != nil {
				s.logger.Debug().Err(err).Str("app", name).Msg("unable to stop app")
			}
		})
	}

	wg.Wait()
	s.watchers.Wait()

	// An app's last line may lack a newline; it is still part of what the
	// app said.
	for _, stream := range s.streams {
		if err := stream.Flush(); err != nil {
			s.logger.Debug().Err(err).Msg("unable to flush app output")
		}
	}

	ctx, cancel := context.WithTimeout(ctx, _terminateTimeout)
	defer cancel()

	errs := make([]error, 0, len(containers))

	var mu sync.Mutex

	for name, c := range containers {
		wg.Go(func() {
			if err := c.Terminate(ctx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("stopping service %s: %w", name, err))
				mu.Unlock()
			}
		})
	}

	wg.Wait()

	for _, f := range s.logs {
		if err := f.Close(); err != nil {
			errs = append(errs, fmt.Errorf("closing %s: %w", f.Name(), err))
		}
	}

	if s.tmp != "" {
		if err := os.RemoveAll(s.tmp); err != nil {
			errs = append(errs, fmt.Errorf("removing temp dir: %w", err))
		}
	}

	// The state file is only ours to remove if it still names this process.
	if st, err := ReadState(s.cfg.Root); err == nil && st.PID == os.Getpid() {
		if err := RemoveState(s.cfg.Root); err != nil {
			errs = append(errs, err)
		}
	}

	if len(apps)+len(containers) > 0 {
		s.out.printf("✓ stopped in %s\n", since(began))
	}

	return errors.Join(errs...)
}

// logExcerpt quotes the end of an app's log for an error message.
func (s *Stack) logExcerpt(path string) string {
	lines := tail(path, _logTail)
	if len(lines) == 0 {
		return ""
	}

	rel, err := filepath.Rel(s.cfg.Root, path)
	if err != nil {
		rel = path
	}

	return fmt.Sprintf("\n  last lines of %s:\n%s", rel, indent(lines, "    "))
}

// environ is the process environment with extra variables over it. Later
// entries win, so the config overrides whatever the shell exported.
func environ(extra map[string]string) []string {
	env := os.Environ()
	for _, k := range slices.Sorted(maps.Keys(extra)) {
		env = append(env, k+"="+extra[k])
	}

	return env
}

func since(t time.Time) time.Duration {
	return time.Since(t).Round(100 * time.Millisecond)
}
