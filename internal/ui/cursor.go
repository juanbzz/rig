package ui

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

// RestoreCursor shows the cursor on f when f is a terminal. A Spinner hides
// it while it runs, and a process killed mid-spin cannot show it again, so
// whatever kills it calls this first. Anything but a terminal is left
// untouched, so no escape code lands in a pipe or a log.
//
// It lives apart from spinner.go, which is kept identical to bbq's.
func RestoreCursor(f *os.File) {
	if f != nil && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(f, _showCursor)
	}
}
