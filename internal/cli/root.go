// Package cli assembles rig's command tree and owns all user-facing output.
//
// Output follows one rule: stdout carries only the result, the stack's URL
// for `up` and the command's own output for `run`, and everything rig says
// about its progress goes to stderr. `URL=$(rig up &)` and a test runner's
// output then stay clean.
package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/j0lvera/rig/internal/config"
	"github.com/j0lvera/rig/internal/container"
	"github.com/j0lvera/rig/internal/stack"
)

// _version is overridden at build time via -ldflags.
var _version = "dev"

// EngineFactory builds the engine services run on.
type EngineFactory func(logger zerolog.Logger) stack.Engine

// CLI holds the state shared by every subcommand.
type CLI struct {
	env    config.Env
	v      *viper.Viper
	engine EngineFactory
}

// New returns a CLI bound to process-level settings, running services in
// Docker.
func New(env config.Env) *CLI {
	return &CLI{env: env, v: viper.New(), engine: container.New}
}

// WithEngine returns a copy of the CLI that runs services on another engine.
func (c *CLI) WithEngine(f EngineFactory) *CLI {
	cp := *c
	cp.engine = f

	return &cp
}

// RootCommand builds the full command tree.
func (c *CLI) RootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "rig",
		Short:         "Disposable app stacks for integration tests",
		Version:       _version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Long: `rig reads .rig.toml at the repository root and turns it into a running,
disposable stack with one URL: containers for the services, the project's own
apps as host processes on free ports, and the steps that migrate and seed in
between. Tests run against the URL; nothing uses a fixed port, so stacks in
different workspaces run side by side.`,
	}

	root.PersistentFlags().BoolP("verbose", "v", c.env.Verbose, "show progress, command and app output, and debug logs on stderr")
	root.PersistentFlags().String("log-level", c.env.LogLevel, "log level (debug, info, warn, error)")
	root.PersistentFlags().StringP("config", "c", "", "path to .rig.toml (default: nearest one up from the current directory)")

	// BindPFlag only fails on a nil flag, and these were defined just above.
	_ = c.v.BindPFlag("verbose", root.PersistentFlags().Lookup("verbose"))
	_ = c.v.BindPFlag("log-level", root.PersistentFlags().Lookup("log-level"))
	_ = c.v.BindPFlag("config", root.PersistentFlags().Lookup("config"))

	root.AddCommand(c.upCommand())
	root.AddCommand(c.runCommand())
	root.AddCommand(c.downCommand())

	return root
}

// Execute runs the command tree.
func (c *CLI) Execute() error {
	return c.RootCommand().Execute()
}

// verbose reports whether rig should narrate what it does. Without it, rig
// prints results and problems only.
func (c *CLI) verbose() bool {
	return c.v.GetBool("verbose")
}

// say writes a progress message, shown only with --verbose.
func (c *CLI) say(w io.Writer, format string, args ...any) {
	if c.verbose() {
		fmt.Fprintf(w, format, args...)
	}
}

// logger builds a stderr logger so stdout stays reserved for command output.
func (c *CLI) logger() zerolog.Logger {
	level, err := zerolog.ParseLevel(c.v.GetString("log-level"))
	if err != nil || level == zerolog.NoLevel {
		level = zerolog.InfoLevel
	}

	if c.v.GetBool("verbose") {
		level = zerolog.DebugLevel
	}

	return zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).
		With().Timestamp().Logger().Level(level)
}

// configPath is the --config flag, or the nearest .rig.toml.
func (c *CLI) configPath() (string, error) {
	if path := c.v.GetString("config"); path != "" {
		return path, nil
	}

	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("reading working directory: %w", err)
	}

	return config.Find(wd)
}

func (c *CLI) loadConfig() (*config.Config, error) {
	path, err := c.configPath()
	if err != nil {
		return nil, err
	}

	return config.Load(path)
}
