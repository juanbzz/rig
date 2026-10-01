// Package config loads and validates a project's .rig.toml.
//
// The file describes a stack in four parts: services rig runs in containers,
// steps it runs once before anything else, apps that are the project's own
// code, and the one app whose URL is the result. Everything a later part needs
// from an earlier one is passed by template, such as {{ services.db.url }}.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/j0lvera/rig/internal/tmpl"
)

// FileName is the per-repository config file.
const FileName = ".rig.toml"

var (
	// ErrNotFound is returned when no .rig.toml exists in the directory or any
	// of its parents.
	ErrNotFound = errors.New("no " + FileName + " found")
	// ErrInvalid is returned when a config file parses but describes a stack
	// rig cannot run.
	ErrInvalid = errors.New("invalid config")
)

// Port modes an app can ask for.
const (
	// PortAuto assigns a free port before anything starts, so every template
	// can refer to it regardless of start order.
	PortAuto = "auto"
)

// Default timeouts, used when the config does not set its own.
const (
	DefaultAppTimeout     = 60 * time.Second
	DefaultServiceTimeout = 2 * time.Minute
)

// Variable names, as they appear inside {{ }}.
const (
	VarTmp  = "tmp"
	VarRoot = "root"
	// VarPort is the app's own port, available only inside that app.
	VarPort = "port"

	FieldURL  = "url"
	FieldHost = "host"
	FieldPort = "port"
)

// ServiceVar names a service's variable, such as services.db.url.
func ServiceVar(name, field string) string {
	return "services." + name + "." + field
}

// AppVar names an app's variable, such as apps.api.url.
func AppVar(name, field string) string {
	return "apps." + name + "." + field
}

// _name is what a service or app may be called. Names end up in template
// paths and environment variable names, so they stay plain.
var _name = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// Duration is a time.Duration written in TOML as a string, such as "90s".
type Duration struct {
	time.Duration
}

// UnmarshalText parses a Go duration string.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}

	d.Duration = v

	return nil
}

// MarshalText writes the duration back in the form it was read.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(d.String()), nil
}

// Ready says when a service's container is ready to use. With neither field
// set, a service is ready once its port accepts connections.
type Ready struct {
	// Log is a regular expression the container's output must match.
	Log string `toml:"log"`
	// Occurrences is how many times Log must match. Zero means once.
	Occurrences int `toml:"occurrences"`
	// HTTP is a path that must answer 2xx on the service's port.
	HTTP string `toml:"http"`
}

// Service is a container rig owns for the life of the stack: an image with a
// port and a ready check. A preset fills in all of it for a known image.
type Service struct {
	Preset  string            `toml:"preset"`
	Image   string            `toml:"image"`
	Port    int               `toml:"port"`
	Env     map[string]string `toml:"env"`
	Ready   Ready             `toml:"ready"`
	Timeout Duration          `toml:"timeout"`
	// URL is how the service is reached, built from {{ host }} and
	// {{ port }}, plus the preset's own variables such as {{ user }}.
	URL string `toml:"url"`

	// Vars holds the preset's extra variables, such as user and password.
	// They are exposed as services.<name>.<var>. Not read from the file.
	Vars map[string]string `toml:"-"`
}

// Step is a one-shot command run in order after services are ready and before
// any app starts: migrations, seeds, asset builds.
type Step struct {
	Name string            `toml:"name"`
	Run  string            `toml:"run"`
	Dir  string            `toml:"dir"`
	Env  map[string]string `toml:"env"`
}

// Label is what a step is called in output: its name, or else the command
// collapsed onto one line.
func (s Step) Label() string {
	if name := strings.TrimSpace(s.Name); name != "" {
		return name
	}

	return strings.Join(strings.Fields(s.Run), " ")
}

// App is the project's own code, run as a host process. It gets a port before
// anything starts and is ready once its health check passes.
type App struct {
	// Build runs once, to completion, before any app starts.
	Build string `toml:"build"`
	// Run starts the app. It must keep running for the life of the stack.
	Run string `toml:"run"`
	// Dir is where both commands run, relative to the config file. Empty
	// means the config file's directory.
	Dir string `toml:"dir"`
	// Port is PortAuto, the default.
	Port string `toml:"port"`
	// Health is an HTTP path that must answer below 400. Empty means the app
	// is ready once its port accepts connections.
	Health  string            `toml:"health"`
	Timeout Duration          `toml:"timeout"`
	Env     map[string]string `toml:"env"`
	// DependsOn lists apps that must be healthy before this one starts.
	DependsOn []string `toml:"depends_on"`
}

// Config is a parsed .rig.toml.
type Config struct {
	// Path is the file the config was read from, and Root its directory.
	// Every relative path in the file is relative to Root.
	Path string `toml:"-"`
	Root string `toml:"-"`

	// Expose names the app whose URL is the stack's result.
	Expose   string             `toml:"expose"`
	Services map[string]Service `toml:"services"`
	Steps    []Step             `toml:"steps"`
	Apps     map[string]App     `toml:"apps"`
}

// Find walks up from dir to the nearest .rig.toml and returns its path.
func Find(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", dir, err)
	}

	for d := abs; ; d = filepath.Dir(d) {
		path := filepath.Join(d, FileName)

		if _, err := os.Stat(path); err == nil {
			return path, nil
		}

		if filepath.Dir(d) == d {
			return "", fmt.Errorf("%w in %s or any parent", ErrNotFound, abs)
		}
	}
}

// Load reads, resolves, and validates the config at path.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving %s: %w", path, err)
	}

	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}

	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", abs, err)
	}

	cfg.Path = abs
	cfg.Root = filepath.Dir(abs)

	return cfg, nil
}

// Parse decodes, resolves presets, and validates a config. Unknown keys are an
// error rather than ignored: a misspelled `helth` would otherwise silently
// turn a health check off.
func Parse(raw []byte) (*Config, error) {
	var cfg Config

	dec := toml.NewDecoder(bytes.NewReader(raw)).DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			problems := make([]string, 0, len(strict.Errors))
			for _, e := range strict.Errors {
				row, _ := e.Position()
				problems = append(problems, fmt.Sprintf("line %d: unknown key %s", row, strings.Join(e.Key(), ".")))
			}

			return nil, invalid(problems)
		}

		var decode *toml.DecodeError
		if errors.As(err, &decode) {
			row, col := decode.Position()
			return nil, fmt.Errorf("%w: line %d, column %d: %s", ErrInvalid, row, col, decode.Error())
		}

		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}

	problems := cfg.resolve()
	problems = append(problems, cfg.validate()...)

	if err := invalid(problems); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// resolve fills each service from its preset and each app from the defaults,
// so everything downstream sees one complete shape.
func (c *Config) resolve() []string {
	var problems []string

	for name, svc := range c.Services {
		if svc.Preset != "" {
			p, ok := _presets[svc.Preset]
			if !ok {
				problems = append(problems, fmt.Sprintf("services.%s: unknown preset %q (known: %s)",
					name, svc.Preset, strings.Join(Presets(), ", ")))
				continue
			}

			svc = p.apply(svc)
		}

		if svc.URL == "" {
			svc.URL = "{{ " + FieldHost + " }}:{{ " + FieldPort + " }}"
		}

		if svc.Timeout.Duration == 0 {
			svc.Timeout.Duration = DefaultServiceTimeout
		}

		c.Services[name] = svc
	}

	for name, app := range c.Apps {
		if app.Port == "" {
			app.Port = PortAuto
		}

		if app.Timeout.Duration == 0 {
			app.Timeout.Duration = DefaultAppTimeout
		}

		c.Apps[name] = app
	}

	return problems
}

// validate returns every problem with the config, so one run reports them
// all rather than one per attempt.
func (c *Config) validate() []string {
	var problems []string

	add := func(format string, args ...any) {
		problems = append(problems, fmt.Sprintf(format, args...))
	}

	switch {
	case len(c.Apps) == 0:
		add("no apps: a stack needs at least one [apps.<name>]")
	case c.Expose == "":
		add("expose: name the app whose URL is the result (one of: %s)", strings.Join(c.AppNames(), ", "))
	default:
		if _, ok := c.Apps[c.Expose]; !ok {
			add("expose: %q is not an app (one of: %s)", c.Expose, strings.Join(c.AppNames(), ", "))
		}
	}

	// Names become RIG_<NAME>_URL, so two that differ only by - and _, or an
	// app and a service sharing one, would write the same variable.
	envNames := map[string]string{}
	checkName := func(kind, name string) {
		if !_name.MatchString(name) {
			add("%s.%s: names must be lowercase letters, digits, - and _, starting with a letter", kind, name)
			return
		}

		key := EnvName(name)
		if other, ok := envNames[key]; ok {
			add("%s.%s: clashes with %s (both become RIG_%s_URL)", kind, name, other, key)
			return
		}

		envNames[key] = kind + "." + name
	}

	for _, name := range c.ServiceNames() {
		checkName("services", name)

		svc := c.Services[name]
		if svc.Preset != "" && !slices.Contains(Presets(), svc.Preset) {
			continue // already reported by resolve
		}

		if svc.Image == "" {
			add("services.%s: set a preset (%s) or an image", name, strings.Join(Presets(), ", "))
		}

		if svc.Port <= 0 {
			add("services.%s: port is required, the port the container listens on", name)
		}

		if svc.Ready.Log != "" && svc.Ready.HTTP != "" {
			add("services.%s: ready takes log or http, not both", name)
		}

		if svc.Ready.Log != "" {
			if _, err := regexp.Compile(svc.Ready.Log); err != nil {
				add("services.%s: ready.log is not a valid regular expression: %v", name, err)
			}
		}

		if svc.Ready.HTTP != "" && !strings.HasPrefix(svc.Ready.HTTP, "/") {
			add("services.%s: ready.http must be a path starting with /", name)
		}

		local := map[string]bool{FieldHost: true, FieldPort: true}
		for k := range svc.Vars {
			local[k] = true
		}

		c.checkRefs(&problems, fmt.Sprintf("services.%s.url", name), svc.URL, local, false)
	}

	for _, name := range c.AppNames() {
		checkName("apps", name)

		app := c.Apps[name]
		prefix := "apps." + name

		if strings.TrimSpace(app.Run) == "" {
			add("%s: run is required", prefix)
		}

		if app.Port != PortAuto {
			add("%s: port %q is not supported; only %q is", prefix, app.Port, PortAuto)
		}

		if app.Health != "" && !strings.HasPrefix(app.Health, "/") {
			add("%s: health must be a path starting with /", prefix)
		}

		for _, dep := range app.DependsOn {
			switch {
			case dep == name:
				add("%s: depends_on lists the app itself", prefix)
			case !c.hasApp(dep):
				add("%s: depends_on %q is not an app", prefix, dep)
			}
		}

		own := map[string]bool{VarPort: true}
		c.checkRefs(&problems, prefix+".build", app.Build, own, true)
		c.checkRefs(&problems, prefix+".run", app.Run, own, true)

		for _, k := range sortedKeys(app.Env) {
			c.checkRefs(&problems, prefix+".env."+k, app.Env[k], own, true)
		}
	}

	for i, step := range c.Steps {
		prefix := fmt.Sprintf("steps[%d]", i)

		if strings.TrimSpace(step.Run) == "" {
			add("%s: run is required", prefix)
		}

		c.checkRefs(&problems, prefix+".run", step.Run, nil, true)

		for _, k := range sortedKeys(step.Env) {
			c.checkRefs(&problems, prefix+".env."+k, step.Env[k], nil, true)
		}
	}

	if cycle := c.dependencyCycle(); len(cycle) > 0 {
		add("depends_on: cycle %s", strings.Join(cycle, " → "))
	}

	return problems
}

// checkRefs reports every placeholder in s that does not name a variable rig
// will have. local holds names that exist only where s is used, such as an
// app's own {{ port }}. A service's url sees only its local names: it is built
// as the service starts, before the stack-wide ones exist.
//
// Checking here rather than at render time means a typo fails before a single
// container starts, instead of a minute in.
func (c *Config) checkRefs(problems *[]string, where, s string, local map[string]bool, global bool) {
	refs, err := tmpl.Refs(s)
	if err != nil {
		*problems = append(*problems, fmt.Sprintf("%s: %v", where, err))
		return
	}

	known := map[string]bool{}
	if global {
		known = c.Vars()
	}

	for _, ref := range refs {
		if local[ref] || known[ref] {
			continue
		}

		if !global {
			*problems = append(*problems, fmt.Sprintf("%s: unknown variable %s (available: %s)",
				where, ref, strings.Join(slices.Sorted(maps.Keys(local)), ", ")))

			continue
		}

		*problems = append(*problems, fmt.Sprintf("%s: unknown variable %s", where, ref))
	}
}

// Vars returns every stack-wide variable name a template may use. Their values
// are filled in as the stack starts.
func (c *Config) Vars() map[string]bool {
	vars := map[string]bool{VarTmp: true, VarRoot: true}

	for name, svc := range c.Services {
		for _, f := range []string{FieldURL, FieldHost, FieldPort} {
			vars[ServiceVar(name, f)] = true
		}

		for k := range svc.Vars {
			vars[ServiceVar(name, k)] = true
		}
	}

	for name := range c.Apps {
		for _, f := range []string{FieldURL, FieldHost, FieldPort} {
			vars[AppVar(name, f)] = true
		}
	}

	return vars
}

// dependencyCycle returns one depends_on cycle as a path of app names, or nil.
func (c *Config) dependencyCycle() []string {
	const (
		unvisited = iota
		visiting
		done
	)

	state := map[string]int{}

	var path []string

	var visit func(name string) []string
	visit = func(name string) []string {
		switch state[name] {
		case visiting:
			start := slices.Index(path, name)
			return append(slices.Clone(path[start:]), name)
		case done:
			return nil
		}

		state[name] = visiting
		path = append(path, name)

		for _, dep := range c.Apps[name].DependsOn {
			if !c.hasApp(dep) {
				continue
			}

			if cycle := visit(dep); cycle != nil {
				return cycle
			}
		}

		path = path[:len(path)-1]
		state[name] = done

		return nil
	}

	for _, name := range c.AppNames() {
		if cycle := visit(name); cycle != nil {
			return cycle
		}
	}

	return nil
}

func (c *Config) hasApp(name string) bool {
	_, ok := c.Apps[name]
	return ok
}

// AppNames returns app names, sorted.
func (c *Config) AppNames() []string {
	return sortedKeys(c.Apps)
}

// ServiceNames returns service names, sorted.
func (c *Config) ServiceNames() []string {
	return sortedKeys(c.Services)
}

// Dir resolves a command's working directory against the config's root.
func (c *Config) Dir(dir string) string {
	switch {
	case dir == "":
		return c.Root
	case filepath.IsAbs(dir):
		return dir
	default:
		return filepath.Join(c.Root, dir)
	}
}

// EnvName turns a service or app name into the middle of its environment
// variable: "web-admin" becomes "WEB_ADMIN", for RIG_WEB_ADMIN_URL.
func EnvName(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

func invalid(problems []string) error {
	if len(problems) == 0 {
		return nil
	}

	return fmt.Errorf("%w:\n  %s", ErrInvalid, strings.Join(problems, "\n  "))
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
