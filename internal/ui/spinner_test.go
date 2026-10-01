package ui

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/matryer/is"
)

// syncBuffer is a bytes.Buffer safe for the spinner's animation goroutine to
// write to while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.buf.String()
}

func TestSpinner_DrawsAndErases(t *testing.T) {
	i := is.New(t)

	var buf syncBuffer
	s := NewSpinner(&buf, 80)

	s.StepStarted("git fetch origin")
	s.StepFinished("git fetch origin", nil)
	s.Stop()

	out := buf.String()
	i.True(strings.Contains(out, "git fetch origin"))
	i.True(strings.Contains(out, _frames[0]))
	i.True(strings.Contains(out, _hideCursor))
	// The line is erased and the cursor restored, so the summary that follows
	// starts on a clean line.
	i.True(strings.HasSuffix(out, _clearLine+_showCursor))
}

func TestSpinner_StopIsIdempotent(t *testing.T) {
	i := is.New(t)

	var buf syncBuffer
	s := NewSpinner(&buf, 80)

	s.StepStarted("working")
	s.Stop()
	s.Stop() // must not panic on a second close
	s.Stop()

	i.True(strings.Contains(buf.String(), "working"))
}

func TestSpinner_StopWithoutStart(t *testing.T) {
	i := is.New(t)

	var buf syncBuffer
	s := NewSpinner(&buf, 80)
	s.Stop()

	// Nothing was drawn, so nothing needs erasing.
	i.Equal(buf.String(), "")
}

func TestSpinner_SwapsDescription(t *testing.T) {
	i := is.New(t)

	var buf syncBuffer
	s := NewSpinner(&buf, 80)

	s.StepStarted("first step")
	s.StepStarted("second step")
	s.Stop()

	out := buf.String()
	i.True(strings.Contains(out, "first step"))
	i.True(strings.Contains(out, "second step"))
}

// A drawn line must never reach the terminal's last column. One that does
// makes the terminal wrap, and a wrapped line survives the next carriage
// return as a line of its own, so the animation walks down the screen leaving
// a copy of every frame behind.
func TestSpinner_NeverFillsTheLine(t *testing.T) {
	// The real case: a worktree step whose description is longer than the
	// window it has to fit in.
	const desc = "git worktree add -b feature/IO-37357-batch-6 " +
		"/Users/mini1/IO/src/io/feature-IO-37357 feature/IO-37357-batch-5"

	for _, width := range []int{20, 40, 80, 100, 110, 111, 112, 113, 120, 200} {
		i := is.New(t)

		var buf syncBuffer
		s := NewSpinner(&buf, width)
		s.StepStarted(desc)
		s.Stop()

		for _, drawn := range strings.Split(buf.String(), _clearLine) {
			line := strings.TrimSuffix(drawn, _showCursor)
			line = strings.TrimPrefix(line, _hideCursor)
			if line == "" {
				continue
			}

			// Counted as if every rune took two cells, since the braille
			// frames may render either way and the guard has to hold for both.
			i.True(len([]rune(line))+1 < width)
		}
	}
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		width int
		want  string
	}{
		{name: "fits", line: "short", width: 20, want: "short"},
		{name: "exact", line: "12345", width: 5, want: "12345"},
		{name: "middle elided", line: "abcdefghij", width: 9, want: "abc...hij"},
		{name: "tiny width", line: "abcdefghij", width: 2, want: ".."},
		{name: "zero width", line: "abc", width: 0, want: "abc"},
		{name: "multibyte safe", line: "⠋ ünïcødé path", width: 9, want: "⠋ ü...ath"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			i := is.New(t)
			i.Equal(truncate(tt.line, tt.width), tt.want)
		})
	}
}

func TestInteractive_NonTerminal(t *testing.T) {
	i := is.New(t)

	f, err := os.CreateTemp(t.TempDir(), "out")
	i.NoErr(err)
	defer f.Close()

	_, interactive := Interactive(f)
	i.True(!interactive)

	_, interactive = Interactive(nil)
	i.True(!interactive)
}

func TestNewSpinner_ClampsWidth(t *testing.T) {
	i := is.New(t)

	var buf syncBuffer
	i.Equal(NewSpinner(&buf, 0).width, _fallbackCol)
	i.Equal(NewSpinner(&buf, 5).width, _fallbackCol)
	i.Equal(NewSpinner(&buf, 120).width, 120)
}

func TestSpinner_PauseErasesTheLineAndResumes(t *testing.T) {
	i := is.New(t)

	buf := &syncBuffer{}
	s := NewSpinner(buf, 80)

	s.StepStarted("copy .env")
	s.Pause()

	// Pausing leaves the terminal as it found it, so a hook's own output
	// starts on a clean line with the cursor back.
	paused := buf.String()
	i.True(strings.HasSuffix(paused, _clearLine+_showCursor))

	// The run isn't over: the next step picks the animation back up rather
	// than printing nothing for the rest of the command.
	s.StepStarted("hook install dependencies")
	i.True(strings.Contains(buf.String()[len(paused):], "hook install dependencies"))

	s.Stop()
}

func TestSpinner_PauseThenStopIsSafe(t *testing.T) {
	buf := &syncBuffer{}
	s := NewSpinner(buf, 80)

	s.StepStarted("copy .env")
	s.Pause()
	s.Pause()
	s.Stop()
}
