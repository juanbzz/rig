// Command rig spins up a disposable app stack for integration tests.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/juanbzz/rig/internal/cli"
	"github.com/juanbzz/rig/internal/config"
	"github.com/juanbzz/rig/internal/ui"
)

func main() {
	if err := run(); err != nil {
		switch {
		case cli.Silent(err):
		case errors.Is(err, context.Canceled):
			fmt.Fprintln(os.Stderr, "rig: interrupted")
		default:
			fmt.Fprintf(os.Stderr, "rig: %v\n", err)
		}

		os.Exit(cli.ExitCode(err))
	}
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	done := make(chan struct{})
	defer close(done)

	go handleSignals(sigs, cancel, done, forceQuit)

	env, err := config.Env{}.LoadEnv()
	if err != nil {
		return err
	}

	return cli.New(env).RootCommand().ExecuteContext(ctx)
}

// handleSignals turns the first signal into a cancelled context, which starts
// an orderly teardown, and hands a second one to quit, for someone who will
// not wait for it. It returns when done closes.
func handleSignals(sigs <-chan os.Signal, cancel context.CancelFunc, done <-chan struct{}, quit func(os.Signal)) {
	select {
	case <-sigs:
		cancel()
	case <-done:
		return
	}

	select {
	case sig := <-sigs:
		quit(sig)
	case <-done:
	}
}

// forceQuit ends rig the way sig would have without a handler, so the exit
// status still says which signal it was. A spinner may have hidden the cursor,
// and a process killed mid-spin cannot show it again, so that comes first.
// Ryuk still removes the containers; app processes may be left behind, and
// the next `rig up` warns about them.
func forceQuit(sig os.Signal) {
	ui.RestoreCursor(os.Stderr)

	signal.Reset()

	s, ok := sig.(syscall.Signal)
	if !ok {
		s = syscall.SIGKILL
	}

	// If the signal cannot be delivered to rig itself, there is nothing left
	// to try; the orderly teardown carries on.
	_ = syscall.Kill(os.Getpid(), s)
}
