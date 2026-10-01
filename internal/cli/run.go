package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// _interruptGrace is how long the command gets to exit on its own once rig is
// interrupted. Ctrl-C reaches it directly from the terminal, and a test
// runner deserves a moment to write its report before it is told again.
const _interruptGrace = 10 * time.Second

func (c *CLI) runCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run [flags] -- COMMAND [ARGS...]",
		Short: "Start the stack, run a command against it, and tear it down",
		Long: `Bring the stack up, run COMMAND with the stack's RIG_* variables in its
environment, then tear the stack down. rig exits with the command's status,
which makes this the form for CI and for test scripts. Only the command's
output is shown unless rig fails or --verbose is given.

The command sees RIG_URL (the exposed app), RIG_<APP>_URL and
RIG_<SERVICE>_URL for everything in the stack, and RIG_STATE, the path to
.rig/state.json.`,
		Example: `  rig run -- playwright test

  rig run -- php artisan dusk

  # a shell, for anything more than one command
  rig run -- sh -c 'pytest tests/e2e && pytest tests/smoke'`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return c.runRun(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), args)
		},
	}

	// Everything after the first non-flag argument belongs to the command, so
	// `rig run playwright test --headed` works without the --.
	cmd.Flags().SetInterspersed(false)

	return cmd
}

func (c *CLI) runRun(ctx context.Context, stdout, stderr io.Writer, args []string) error {
	if args[0] == "" {
		return usageErrorf("run needs a command, as in: rig run -- playwright test")
	}

	sp := c.spinner(stderr)

	st, err := c.startStack(ctx, stderr, sp)
	stopSpinner(sp)

	if err != nil {
		return err
	}

	runErr := c.runAgainst(ctx, stdout, stderr, args, st.Env(), st.Crashed())

	downErr := st.Down(context.WithoutCancel(ctx))
	stopSpinner(sp)

	// The command's status is returned as is when teardown succeeds, so rig
	// exits with it silently.
	if downErr != nil {
		return errors.Join(runErr, downErr)
	}

	return runErr
}

// runAgainst runs the command, sharing rig's stdout and stderr, and returns a
// CommandExitError carrying its status when it fails.
func (c *CLI) runAgainst(ctx context.Context, stdout, stderr io.Writer, args, env []string, crashed <-chan error) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = append(os.Environ(), env...)

	// At a terminal the command stays in rig's process group, the terminal's
	// foreground group, so Ctrl-C reaches it and everything it started, and
	// it can still read the terminal. Elsewhere, as in CI, nothing delivers a
	// signal to the whole tree, so the command gets a group of its own that
	// rig can signal: a test runner's browsers stop with it.
	group := !isTerminal(os.Stdin)
	if group {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting %s: %w", args[0], err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var err error

wait:
	for {
		select {
		case err = <-done:
			break wait
		case crash := <-crashed:
			// The command is left to fail on its own, with its own report of
			// what broke; rig only says why.
			fmt.Fprintf(stderr, "! %v\n", crash)
		case <-ctx.Done():
			err = stopCommand(cmd, done, group, stderr)
			break wait
		}
	}

	if err == nil {
		return nil
	}

	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return &CommandExitError{Code: exitCode(exit)}
	}

	return fmt.Errorf("running %s: %w", args[0], err)
}

// stopCommand ends the command after rig is interrupted. At a terminal it
// waits first, since Ctrl-C already reached the command, and escalates to
// SIGTERM and then SIGKILL. With a group of its own, nothing else signalled
// it, so SIGTERM goes to the whole group straight away.
func stopCommand(cmd *exec.Cmd, done <-chan error, group bool, stderr io.Writer) error {
	steps := []syscall.Signal{0, syscall.SIGTERM, syscall.SIGKILL}
	if group {
		steps = steps[1:]
	}

	for _, sig := range steps {
		if sig != 0 {
			if err := signalCommand(cmd, sig, group); err != nil {
				fmt.Fprintf(stderr, "! %v\n", err)
			}
		}

		select {
		case err := <-done:
			return err
		case <-time.After(_interruptGrace):
		}
	}

	return <-done
}

// signalCommand sends sig to the command, or to its whole group. A command
// that has already exited is not an error.
func signalCommand(cmd *exec.Cmd, sig syscall.Signal, group bool) error {
	var err error
	if group {
		err = syscall.Kill(-cmd.Process.Pid, sig)
	} else {
		err = cmd.Process.Signal(sig)
	}

	if err == nil || errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
		return nil
	}

	return fmt.Errorf("signalling %s: %w", cmd.Path, err)
}

// isTerminal reports whether f is a terminal. A mode check is not enough:
// /dev/null, a common stdin in CI, is a character device too.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// exitCode follows the shell's convention: a command killed by a signal exits
// 128 plus the signal number.
func exitCode(exit *exec.ExitError) int {
	if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		return 128 + int(status.Signal())
	}

	return exit.ExitCode()
}
