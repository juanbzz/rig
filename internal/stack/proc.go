package stack

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"
)

// _shell runs every command in the config. A shell rather than an argv keeps
// a value honest about what it is, a line you could have typed, and lets a
// project write `cd apps/api && go build` without rig inventing a syntax.
const _shell = "sh"

// _pollInterval is how often a stopping process group is checked for exit.
const _pollInterval = 50 * time.Millisecond

// procSpec describes one command to run.
type procSpec struct {
	command string
	dir     string
	env     []string
	stdout  io.Writer
	stderr  io.Writer
	// waitDelay bounds how long Wait waits for output pipes to close after
	// the process exits, so a grandchild holding stdout cannot hang rig.
	waitDelay time.Duration
}

// proc is a command running in its own process group.
//
// The group is the point: `go run`, `bun run`, and `php artisan serve` each
// start a child that does the real work, and signalling only the shell leaves
// that child serving on its port after rig has gone. Signalling the group
// reaches all of them.
type proc struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

// startProc starts spec in a new process group. The returned proc is reaped in
// the background; Done closes when it has exited.
func startProc(spec procSpec) (*proc, error) {
	cmd := exec.Command(_shell, "-c", spec.command)
	cmd.Dir = spec.dir
	cmd.Env = spec.env
	cmd.Stdout = spec.stdout
	cmd.Stderr = spec.stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = spec.waitDelay

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %q: %w", spec.command, err)
	}

	p := &proc{cmd: cmd, done: make(chan struct{})}

	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()

	return p, nil
}

// PID is the process group leader's pid, which is also the group id.
func (p *proc) PID() int {
	return p.cmd.Process.Pid
}

// Done closes once the group leader has exited and been reaped.
func (p *proc) Done() <-chan struct{} {
	return p.done
}

// Err is the leader's exit error. It is only meaningful after Done closes.
func (p *proc) Err() error {
	return p.err
}

// Stop ends the whole process group: SIGTERM first, then SIGKILL for whatever
// is still alive after grace. It returns once the leader is reaped, with any
// error from signalling the group; the group is gone either way, unless it
// could not be signalled at all.
//
// The leader exiting is not enough to stop waiting. A shell can exit on
// SIGTERM while the server it started is still finishing requests, and that
// server is what holds the port.
func (p *proc) Stop(grace time.Duration) error {
	pgid := p.PID()

	termErr := signalGroup(pgid, syscall.SIGTERM)

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if p.exited() && !groupAlive(pgid) {
			return termErr
		}

		time.Sleep(_pollInterval)
	}

	killErr := signalGroup(pgid, syscall.SIGKILL)
	<-p.done

	return errors.Join(termErr, killErr)
}

func (p *proc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// signalGroup sends sig to every process in the group. A group that is
// already gone is not an error.
func signalGroup(pgid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signalling process group %d: %w", pgid, err)
	}

	return nil
}

// groupAlive reports whether any process in the group still exists.
func groupAlive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// processAlive reports whether a process with this pid exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}

	err := syscall.Kill(pid, 0)

	return err == nil || errors.Is(err, syscall.EPERM)
}

// exitStatus describes how a process ended, for an error message.
func exitStatus(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return fmt.Sprintf("was killed by signal %d (%s)", int(status.Signal()), status.Signal())
		}

		return fmt.Sprintf("exited with status %d", exit.ExitCode())
	}

	if err == nil {
		return "exited with status 0"
	}

	return "failed: " + err.Error()
}
