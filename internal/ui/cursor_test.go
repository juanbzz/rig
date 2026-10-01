package ui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/matryer/is"
)

func TestRestoreCursor_LeavesFilesAlone(t *testing.T) {
	i := is.New(t)

	f, err := os.Create(filepath.Join(t.TempDir(), "stderr"))
	i.NoErr(err)
	defer f.Close()

	RestoreCursor(f)
	RestoreCursor(nil)

	info, err := f.Stat()
	i.NoErr(err)
	i.Equal(info.Size(), int64(0))
}
