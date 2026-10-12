//go:build cgo

// fixture_format_version_test.go — the committed `.ct` recordings are in the
// trace format the recorder writes.
//
// The recordings under `testdata/` are captured output of producers in
// sibling repos and are committed rather than made by the tests, so nothing
// else notices when the trace format moves on and they stay behind: the
// replay paths here read the members they need and never look at
// `meta.dat`'s version, while every CodeTracer reader accepts only the
// current one.  This compares each recording's `meta.dat` version with the
// version the linked trace writer puts in a fresh recording.
//
// No mocks: the reference version comes from a real container written by
// the Nim FFI writer, and the recordings are read with the CTFS reader.
package main

import (
	"encoding/binary"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/tracewriter"
)

// metaDatVersion returns the `meta.dat` format version of the container at
// path: the u16 that follows the "CTMD" magic.  ok is false for a container
// with no `meta.dat` (a bare CTFS layout fixture).
func metaDatVersion(t *testing.T, path string) (version uint16, ok bool) {
	t.Helper()
	c, err := ctfs.Open(path)
	require.NoError(t, err, "opening %s", path)
	if !c.Has("meta.dat") {
		return 0, false
	}
	m, err := c.ReadFile("meta.dat")
	require.NoError(t, err, "reading meta.dat of %s", path)
	require.True(t, len(m) >= 6 && string(m[:4]) == "CTMD",
		"%s: meta.dat does not start with the CTMD magic", path)
	return binary.LittleEndian.Uint16(m[4:6]), true
}

func TestCommittedRecordingsAreInTheWrittenFormatVersion(t *testing.T) {
	dir := t.TempDir()
	w := tracewriter.NewCtfsTraceWriter()
	w.RegisterStep("/src/main.wat", 1)
	require.NoError(t, w.ProduceTrace(dir, "reference", "/src"))
	written, ok := metaDatVersion(t, filepath.Join(dir, "reference.ct"))
	require.True(t, ok, "a fresh recording has no meta.dat")

	var checked []string
	err := filepath.WalkDir("testdata", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".ct") {
			return err
		}
		v, ok := metaDatVersion(t, path)
		if !ok {
			return nil
		}
		checked = append(checked, path)
		if v != written {
			t.Errorf("%s carries meta.dat version %d; the trace writer writes "+
				"version %d, the only one CodeTracer readers accept", path, v, written)
		}
		return nil
	})
	require.NoError(t, err)
	require.True(t, len(checked) > 0, "no committed .ct recording carries a meta.dat")
}
