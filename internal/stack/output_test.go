package stack

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/matryer/is"
)

func TestPrefixWriter_HoldsPartialLines(t *testing.T) {
	i := is.New(t)

	var out bytes.Buffer
	w := newPrefixWriter(&out, "api │ ")

	_, err := w.Write([]byte("one\ntw"))
	i.NoErr(err)
	i.Equal(out.String(), "api │ one\n")

	_, err = w.Write([]byte("o\nthree\n"))
	i.NoErr(err)
	i.Equal(out.String(), "api │ one\napi │ two\napi │ three\n")
}

func TestPrefixWriter_FlushWritesTheRest(t *testing.T) {
	i := is.New(t)

	var out bytes.Buffer
	w := newPrefixWriter(&out, "api │ ")

	_, err := w.Write([]byte("done\nexiting"))
	i.NoErr(err)
	i.NoErr(w.Flush())
	i.Equal(out.String(), "api │ done\napi │ exiting\n")

	i.NoErr(w.Flush()) // nothing held back, nothing written
	i.Equal(out.String(), "api │ done\napi │ exiting\n")
}

func TestTail(t *testing.T) {
	i := is.New(t)

	path := filepath.Join(t.TempDir(), "app.log")

	var b bytes.Buffer
	for n := range 50 {
		b.WriteString("line " + strconv.Itoa(n) + "\n")
	}

	i.NoErr(os.WriteFile(path, b.Bytes(), 0o644))

	got := tail(path, 3)
	i.Equal(got, []string{"line 47", "line 48", "line 49"})

	i.Equal(len(tail(filepath.Join(t.TempDir(), "missing.log"), 3)), 0)
}

func TestFreePorts_Distinct(t *testing.T) {
	i := is.New(t)

	ports, err := freePorts(20)
	i.NoErr(err)

	seen := map[int]bool{}
	for _, p := range ports {
		i.True(!seen[p])
		seen[p] = true
	}
}
