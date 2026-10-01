// Package ui renders interactive terminal feedback. Everything here writes to
// stderr so stdout stays clean for the command's real output.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// _frames is a braille dot cycle, one code point per frame so the animation
// redraws in place without disturbing the line.
var _frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	_interval    = 80 * time.Millisecond
	_clearLine   = "\r\033[K"
	_hideCursor  = "\033[?25l"
	_showCursor  = "\033[?25h"
	_minColumns  = 20
	_fallbackCol = 80

	// _reserved is how many columns a drawn line keeps clear of the terminal's
	// right edge. Two of them, because the braille frames sit in Unicode's
	// ambiguous width class and a terminal is free to render them two cells
	// wide rather than one. A line that reaches the final cell makes the
	// terminal wrap, and a wrapped line survives the next \r as a line of its
	// own, so the animation walks down the screen instead of repainting.
	_reserved = 2
)

// Spinner animates a single status line while work runs, replacing the text as
// each step begins and erasing itself entirely when stopped.
type Spinner struct {
	w io.Writer
	// file is w when it is a terminal, so the width can be re-read as the
	// animation runs. Nil for any other writer.
	file  *os.File
	width int

	mu      sync.Mutex
	desc    string
	frame   int
	running bool

	done chan struct{}
	wg   sync.WaitGroup
}

// NewSpinner returns a Spinner drawing to w, fitting its lines to width
// columns. When w is a terminal the width is re-read as the animation runs, so
// resizing the window mid-command cannot strand a line that no longer fits.
func NewSpinner(w io.Writer, width int) *Spinner {
	if width < _minColumns {
		width = _fallbackCol
	}

	s := &Spinner{w: w, width: width, done: make(chan struct{})}
	if f, ok := w.(*os.File); ok {
		s.file = f
	}

	return s
}

// StepStarted swaps in a new status line, starting the animation on first use
// and restarting it after a Pause.
func (s *Spinner) StepStarted(desc string) {
	s.mu.Lock()
	s.desc = desc
	start := !s.running
	s.running = true
	done := s.done
	s.mu.Unlock()

	if start {
		fmt.Fprint(s.w, _hideCursor)
		s.wg.Add(1)

		go s.loop(done)
	}

	s.draw()
}

// StepFinished is part of the step lifecycle; the spinner simply moves on to
// whatever comes next, and Stop performs the cleanup.
func (s *Spinner) StepFinished(string, error) {}

// Stop halts the animation and erases the status line. It is safe to call more
// than once.
func (s *Spinner) Stop() { s.halt() }

// Pause hands the terminal line to a step that prints output of its own. It
// erases the line without ending the run: the next StepStarted picks the
// animation back up where it left off.
func (s *Spinner) Pause() { s.halt() }

// halt stops the animation goroutine and clears the line, leaving the spinner
// ready to run again. The done channel is replaced rather than reused, since a
// closed one would end the next loop the moment it started.
func (s *Spinner) halt() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()

		return
	}
	s.running = false
	done := s.done
	s.done = make(chan struct{})
	s.mu.Unlock()

	close(done)
	s.wg.Wait()

	fmt.Fprint(s.w, _clearLine+_showCursor)
}

// loop advances the animation until halt closes the done channel it was
// started with.
func (s *Spinner) loop(done chan struct{}) {
	defer s.wg.Done()

	ticker := time.NewTicker(_interval)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			s.mu.Lock()
			s.frame = (s.frame + 1) % len(_frames)
			s.mu.Unlock()

			s.draw()
		}
	}
}

// draw repaints the status line in place.
func (s *Spinner) draw() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return
	}

	line := _frames[s.frame] + " " + s.desc
	fmt.Fprint(s.w, _clearLine+truncate(line, s.columns()-_reserved))
}

// columns reports the width to draw within: the terminal's current width when
// there is one, and otherwise the width the spinner was built with.
func (s *Spinner) columns() int {
	if s.file == nil {
		return s.width
	}

	width, _, err := term.GetSize(int(s.file.Fd()))
	if err != nil || width < _minColumns {
		return s.width
	}

	return width
}

// truncate shortens a line to at most width display cells, ellipsizing the
// middle so both the verb and its target stay readable.
func truncate(line string, width int) string {
	runes := []rune(line)
	if width <= 0 || len(runes) <= width {
		return line
	}
	if width <= 3 {
		return strings.Repeat(".", width)
	}

	keep := width - 3
	head := keep / 2
	tail := keep - head

	return string(runes[:head]) + "..." + string(runes[len(runes)-tail:])
}

// Interactive reports whether f is a terminal that can host an animation, and
// returns its width.
func Interactive(f *os.File) (int, bool) {
	if f == nil {
		return 0, false
	}

	fd := int(f.Fd())
	if !term.IsTerminal(fd) {
		return 0, false
	}

	// A dumb terminal, or CI, won't render the escape sequences meaningfully.
	if os.Getenv("TERM") == "dumb" || os.Getenv("CI") != "" {
		return 0, false
	}

	width, _, err := term.GetSize(fd)
	if err != nil {
		return _fallbackCol, true
	}

	return width, true
}
