package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/j0lvera/rig/internal/stack"
)

// _downPoll is how often `rig down` checks whether the stack has stopped.
const _downPoll = 100 * time.Millisecond

// ErrStillRunning is returned when the rig process outlives `rig down`'s
// timeout.
var ErrStillRunning = errors.New("rig is still running")

func (c *CLI) downCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop a running `rig up` from another shell",
		Long: `Signal the rig process that owns this project's stack and wait for it to
tear everything down. It prints nothing unless --verbose is given, and it is
not an error when nothing is running.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			timeout, err := cmd.Flags().GetDuration("timeout")
			if err != nil {
				return err
			}

			return c.runDown(cmd.Context(), cmd.ErrOrStderr(), timeout)
		},
	}

	cmd.Flags().Duration("timeout", time.Minute, "how long to wait for the stack to stop")

	return cmd
}

func (c *CLI) runDown(ctx context.Context, stderr io.Writer, timeout time.Duration) error {
	// Only the root is needed, so a config broken since `up` started does
	// not stop the stack from coming down.
	path, err := c.configPath()
	if err != nil {
		return err
	}

	root := filepath.Dir(path)

	st, err := stack.ReadState(root)
	if errors.Is(err, stack.ErrNotRunning) {
		c.say(stderr, "no rig running in %s\n", root)
		return nil
	}

	if err != nil {
		return err
	}

	if !st.Alive() {
		if err := stack.RemoveState(root); err != nil {
			return err
		}

		c.say(stderr, "no rig running in %s (removed state left by pid %d)\n", root, st.PID)

		return nil
	}

	if err := syscall.Kill(st.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signalling rig (pid %d): %w", st.PID, err)
	}

	c.say(stderr, "→ stopping rig (pid %d)\n", st.PID)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for st.Alive() {
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w after %s (pid %d)", ErrStillRunning, timeout, st.PID)
		case <-time.After(_downPoll):
		}
	}

	c.say(stderr, "✓ stopped\n")

	return nil
}
