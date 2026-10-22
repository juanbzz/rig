package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/juanbzz/rig/internal/config"
)

// Exit codes follow the UNIX convention: 1 for a failed operation, 2 for a
// malformed invocation, and 130 for one interrupted by Ctrl-C.
const (
	ExitError       = 1
	ExitUsage       = 2
	ExitInterrupted = 130
)

// UsageError marks an error caused by how the command was invoked rather than
// by the work it attempted.
type UsageError struct {
	msg string
}

func (e *UsageError) Error() string {
	return e.msg
}

func usageErrorf(format string, args ...any) error {
	return &UsageError{msg: fmt.Sprintf(format, args...)}
}

// CommandExitError carries the exit status of the command `rig run` ran, so
// rig exits with it. The command has already said what went wrong.
type CommandExitError struct {
	Code int
}

func (e *CommandExitError) Error() string {
	return "command exited with status " + strconv.Itoa(e.Code)
}

// Silent reports whether err should end the process without a message: it is
// the command's exit status and nothing else. Joined with a teardown failure,
// it is not, and the failure still gets printed.
func Silent(err error) bool {
	var exit *CommandExitError
	return errors.As(err, &exit) && err == error(exit) //nolint:errorlint // identity, not equivalence
}

// ExitCode maps an error onto the process exit status.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}

	var (
		usage *UsageError
		exit  *CommandExitError
	)

	switch {
	case errors.As(err, &exit):
		return exit.Code
	case errors.Is(err, context.Canceled):
		return ExitInterrupted
	case errors.As(err, &usage),
		errors.Is(err, config.ErrNotFound),
		errors.Is(err, config.ErrInvalid):
		return ExitUsage
	default:
		return ExitError
	}
}
