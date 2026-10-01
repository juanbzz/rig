package stack

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

// syncWriter serializes writes from concurrent services and apps, so two
// progress lines never interleave mid-line.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.w.Write(p)
}

// printf writes a progress line. A write error is dropped: progress output is
// a courtesy, and a closed stderr is no reason to abandon a teardown.
func (s *syncWriter) printf(format string, args ...any) {
	_, _ = fmt.Fprintf(s, format, args...)
}

// prefixWriter writes each complete line with a prefix, holding back a partial
// line until its newline arrives. It lets several apps stream into one
// terminal and still be told apart.
type prefixWriter struct {
	mu     sync.Mutex
	prefix string
	out    io.Writer
	buf    []byte
}

func newPrefixWriter(out io.Writer, prefix string) *prefixWriter {
	return &prefixWriter{prefix: prefix, out: out}
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.buf = append(p.buf, b...)

	for {
		i := bytes.IndexByte(p.buf, '\n')
		if i < 0 {
			break
		}

		line := p.buf[:i+1]
		if _, err := io.WriteString(p.out, p.prefix+string(line)); err != nil {
			return len(b), err
		}

		p.buf = p.buf[i+1:]
	}

	return len(b), nil
}

// Flush writes a held-back partial line, newline-terminated.
func (p *prefixWriter) Flush() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.buf) == 0 {
		return nil
	}

	_, err := io.WriteString(p.out, p.prefix+string(p.buf)+"\n")
	p.buf = nil

	return err
}

// tail returns up to n final lines of a file, for an error message about an
// app that would not start. A file that cannot be read yields nothing: the
// error being reported matters more than its context.
func tail(path string, n int) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	lines := make([]string, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)

	for sc.Scan() {
		if len(lines) == n {
			lines = lines[1:]
		}

		lines = append(lines, sc.Text())
	}

	return lines
}

// indent prefixes every line, for nesting output under a heading.
func indent(lines []string, prefix string) string {
	var b strings.Builder

	for _, l := range lines {
		b.WriteString(prefix)
		b.WriteString(l)
		b.WriteByte('\n')
	}

	return strings.TrimSuffix(b.String(), "\n")
}
