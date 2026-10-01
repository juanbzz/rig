package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/j0lvera/rig/internal/config"
	"github.com/j0lvera/rig/internal/stack"
)

func (c *CLI) upCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start the stack and block until Ctrl-C",
		Long: `Start every service, run every step, start every app, and block until
interrupted. Once everything is healthy, the exposed app's URL is printed to
stdout, and that is all rig prints unless something goes wrong;
.rig/state.json describes the whole stack, and .rig/logs/ holds what every
command said. --verbose shows it all as it happens.

Stop it with Ctrl-C, or with ` + "`rig down`" + ` from another shell. If an app exits
on its own, the stack comes down and rig exits non-zero.`,
		Example: `  rig up

  # watch everything: progress, migrations, builds, and app output
  rig up --verbose

  # in the background, for an agent or a second shell
  rig up > url.txt &`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return c.runUp(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}

	return cmd
}

func (c *CLI) runUp(ctx context.Context, stdout, stderr io.Writer) error {
	st, err := c.startStack(ctx, stderr)
	if err != nil {
		return err
	}

	if c.verbose() {
		if err := printSummary(stderr, st.State()); err != nil {
			return errors.Join(err, st.Down(context.WithoutCancel(ctx)))
		}

		fmt.Fprintln(stderr, "Ctrl-C or `rig down` to stop")
	}

	fmt.Fprintln(stdout, st.State().URL)

	// Being stopped is how `up` is meant to end, so it is not an error. An
	// app dying on its own is.
	var crashed error
	select {
	case <-ctx.Done():
	case crashed = <-st.Crashed():
	}

	return errors.Join(crashed, st.Down(context.WithoutCancel(ctx)))
}

// startStack loads the config and brings the stack up.
func (c *CLI) startStack(ctx context.Context, stderr io.Writer) (*stack.Stack, error) {
	cfg, err := c.loadConfig()
	if err != nil {
		return nil, err
	}

	logger := c.logger()

	return stack.Up(ctx, cfg, stack.Options{
		Engine:  c.engine(logger),
		Out:     stderr,
		Verbose: c.verbose(),
		Logger:  logger,
	})
}

// printSummary lists where everything in the stack is reachable.
func printSummary(w io.Writer, st stack.State) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	for _, name := range slices.Sorted(maps.Keys(st.Apps)) {
		app := st.Apps[name]
		fmt.Fprintf(tw, "  %s\t%s\tRIG_%s_URL\n", name, app.URL, config.EnvName(name))
	}

	for _, name := range slices.Sorted(maps.Keys(st.Services)) {
		svc := st.Services[name]
		fmt.Fprintf(tw, "  %s\t%s\tRIG_%s_URL\n", name, svc.URL, config.EnvName(name))
	}

	fmt.Fprintf(tw, "  state\t%s\n", relative(st.Root, stack.StatePath(st.Root)))
	fmt.Fprintf(tw, "  logs\t%s\n", relative(st.Root, filepath.Join(st.Root, stack.DirName, stack.LogsDir))+"/")

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	return nil
}

// relative shortens a path under root for display.
func relative(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}

	return rel
}
