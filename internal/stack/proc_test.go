package stack

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestProc_StopKillsTheGroup covers the case the process group exists for: a
// shell that starts a child, the way `go run` and `bun run` do. Stopping
// must reach the child, not only the shell.
func TestProc_StopKillsTheGroup(t *testing.T) {
	i := is.New(t)

	out := &lockedBuffer{b: &bytes.Buffer{}}

	p, err := startProc(procSpec{
		command: "sleep 300 & echo $!; wait",
		stdout:  out,
		stderr:  out,
	})
	i.NoErr(err)

	child := waitForPID(t, out)
	i.True(processAlive(child))

	i.NoErr(p.Stop(2 * time.Second))

	i.True(!groupAlive(p.PID()))
	i.True(!processAlive(child))
}

// TestProc_StopEscalates covers a process that ignores SIGTERM.
func TestProc_StopEscalates(t *testing.T) {
	i := is.New(t)

	out := &lockedBuffer{b: &bytes.Buffer{}}

	p, err := startProc(procSpec{
		command: "trap '' TERM; echo ready; while true; do sleep 0.1; done",
		stdout:  out,
	})
	i.NoErr(err)

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "ready") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	began := time.Now()
	i.NoErr(p.Stop(300 * time.Millisecond))

	i.True(time.Since(began) < 3*time.Second)
	i.True(!groupAlive(p.PID()))
}

func TestExitStatus(t *testing.T) {
	i := is.New(t)

	p, err := startProc(procSpec{command: "exit 5"})
	i.NoErr(err)
	<-p.Done()

	i.Equal(exitStatus(p.Err()), "exited with status 5")
	i.Equal(exitStatus(nil), "exited with status 0")
}

func waitForPID(t *testing.T, out *lockedBuffer) int {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if line, _, ok := strings.Cut(out.String(), "\n"); ok {
			pid, err := strconv.Atoi(strings.TrimSpace(line))
			if err != nil {
				t.Fatalf("reading child pid: %v", err)
			}

			return pid
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("child never reported its pid")

	return 0
}
