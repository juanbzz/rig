// Command rig spins up a disposable app stack for integration tests.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/j0lvera/rig/internal/cli"
	"github.com/j0lvera/rig/internal/config"
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The first signal starts an orderly teardown. Restoring the default
	// handlers then lets a second Ctrl-C kill rig outright; Ryuk still
	// removes the containers.
	context.AfterFunc(ctx, stop)

	env, err := config.Env{}.LoadEnv()
	if err != nil {
		return err
	}

	return cli.New(env).RootCommand().ExecuteContext(ctx)
}
