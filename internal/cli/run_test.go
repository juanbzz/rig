package cli

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/matryer/is"
)

// TestStopCommand_Group covers rig run outside a terminal: the command leads
// its own group, and stopping it reaches what it started, not only itself.
func TestStopCommand_Group(t *testing.T) {
	i := is.New(t)

	var out syncBuffer

	cmd := exec.Command("sh", "-c", "sleep 300 & echo $!; wait")
	cmd.Stdout = &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	i.NoErr(cmd.Start())

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	child := readPID(t, &out)

	began := time.Now()
	err := stopCommand(cmd, done, true, &bytes.Buffer{})
	i.True(err != nil) // killed by SIGTERM
	i.True(time.Since(began) < _interruptGrace)

	// The child was signalled with its parent, so it is gone or going.
	deadline := time.Now().Add(2 * time.Second)
	for syscall.Kill(child, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	i.True(syscall.Kill(child, 0) != nil)
}

func TestSignalCommand_AlreadyExited(t *testing.T) {
	i := is.New(t)

	cmd := exec.Command("true")
	i.NoErr(cmd.Run())

	i.NoErr(signalCommand(cmd, syscall.SIGTERM, false))
}

func readPID(t *testing.T, out *syncBuffer) int {
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
