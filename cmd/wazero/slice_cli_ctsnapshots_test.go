//go:build ctsnapshots

package main

// The `--slice-dir` CLI surface — `WASM-Replay-Snapshots-And-Slices.md` §7.
//
// No mocks: real slice containers on disk, loaded back through the shipped
// reader and materialised by a second run of the shipped binary.

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasmsnapshot"
)

// TestSliceDirEmitsIndependentlyMaterialisableContainers covers the whole flag
// path: the recording is split, the manifest describes the set, and one slice
// on its own materialises its range.
func TestSliceDirEmitsIndependentlyMaterialisableContainers(t *testing.T) {
	require.True(t, snapshotsAvailable)
	src := repeatRecording(t, 4)
	dir := filepath.Join(t.TempDir(), "slices")

	code, stdout, stderr := runMain(t, "", []string{
		"run",
		"--boundary-log=" + src,
		"--slice-dir=" + dir,
		"--slice-every=2",
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 0, code, "stderr:\n%s", stderr)
	require.True(t, bytes.Contains([]byte(stdout), []byte("wrote 2 slice(s)")),
		"the run did not report the slice set: %s", stdout)

	m, err := wasmsnapshot.LoadSliceManifest(filepath.Join(dir, wasmsnapshot.SliceManifestName))
	require.NoError(t, err)
	require.Equal(t, 2, len(m.Slices))
	require.Equal(t, 5, m.TotalPoints)
	require.Equal(t, 0, m.Slices[0].BasePoint)
	require.Equal(t, 2, m.Slices[0].EndPoint)
	require.Equal(t, 2, m.Slices[1].BasePoint)
	require.Equal(t, 4, m.Slices[1].EndPoint)

	// Nothing else in the directory, and every slice is a plain container.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Equal(t, 3, len(entries)) // two slices plus the manifest

	for _, s := range m.Slices {
		path := filepath.Join(dir, s.File)
		c, err := ctfs.Open(path)
		require.NoError(t, err)
		require.True(t, c.Has("meta.dat"))
		for _, ns := range wasmsnapshot.NamespaceNames() {
			require.True(t, c.Has(ns), "slice %d is missing %s", s.Index, ns)
		}
	}

	// Slice 1, alone, materialises quiescent points [2,4] — via the CLI, with
	// only that one file present.
	alone := filepath.Join(t.TempDir(), "alone")
	require.NoError(t, os.MkdirAll(alone, 0o755))
	raw, err := os.ReadFile(filepath.Join(dir, m.Slices[1].File))
	require.NoError(t, err)
	slicePath := filepath.Join(alone, m.Slices[1].File)
	require.NoError(t, os.WriteFile(slicePath, raw, 0o644))

	out := t.TempDir()
	code, stdout, stderr = runMain(t, "", []string{
		"run",
		"--boundary-log=" + src,
		"--snapshot-source=" + slicePath,
		"--seek-from=2",
		"--seek-to=4",
		"--out-dir=" + out,
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 0, code, "stderr:\n%s", stderr)
	require.True(t, bytes.Contains([]byte(stdout), []byte("materialised quiescent-point range [2,4]")),
		"the seek did not report the range: %s", stdout)
	require.True(t, ctfsHas(t, filepath.Join(out, "balance_calc.ct"), "steps.dat"))
}

// TestSliceDirWithBoundaryStreamSealsSlicesDuringTheRecording is §2 and §7
// together, through the CLI: the recording arrives while the run is under way
// and slice containers appear as it goes.
func TestSliceDirWithBoundaryStreamSealsSlicesDuringTheRecording(t *testing.T) {
	src := repeatRecording(t, 6)
	raw, err := os.ReadFile(filepath.Join(src, "trace.json"))
	require.NoError(t, err)

	live := filepath.Join(t.TempDir(), "frontend-wasm.ct")
	require.NoError(t, os.MkdirAll(live, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(live, "trace.json"), nil, 0o644))
	for _, f := range []string{"trace_metadata.json", "trace_paths.json"} {
		b, err := os.ReadFile(filepath.Join(src, f))
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(live, f), b, 0o644))
	}
	marker := filepath.Join(t.TempDir(), "done")

	var wg sync.WaitGroup
	dribble(t, &wg, filepath.Join(live, "trace.json"), marker, raw)

	dir := filepath.Join(t.TempDir(), "slices")
	code, stdout, stderr := runMain(t, "", []string{
		"run",
		"--boundary-log=" + live,
		"--boundary-stream=" + filepath.Join(live, "trace.json"),
		"--stream-done=" + marker,
		"--slice-dir=" + dir,
		"--slice-every=2",
		"testdata/boundary-log/balance_calc.wasm",
	})
	wg.Wait()
	require.Equal(t, 0, code, "stderr:\n%s", stderr)
	require.True(t, bytes.Contains([]byte(stdout), []byte("wrote 3 slice(s)")), stdout)

	m, err := wasmsnapshot.LoadSliceManifest(filepath.Join(dir, wasmsnapshot.SliceManifestName))
	require.NoError(t, err)
	require.Equal(t, 3, len(m.Slices))
	for i, s := range m.Slices {
		require.Equal(t, 2*i, s.BasePoint)
		require.Equal(t, 2*i+2, s.EndPoint)
		set, diag, err := wasmsnapshot.Load(filepath.Join(dir, s.File), false)
		require.NoError(t, err)
		require.Equal(t, "", diag)
		base, end, hasBase := set.Range()
		require.True(t, hasBase)
		require.Equal(t, s.BasePoint, base)
		require.Equal(t, s.EndPoint, end)
	}
}

// TestSliceFlagsRefuseIncoherentCombinations: each refusal explains what the
// two flags would mean together, rather than silently preferring one.
func TestSliceFlagsRefuseIncoherentCombinations(t *testing.T) {
	src := repeatRecording(t, 2)
	for _, tc := range []struct {
		name  string
		args  []string
		wants string
	}{
		{"slice-every without slice-dir", []string{"--slice-every=2"}, "need --slice-dir"},
		{"slice-dir with seek", []string{
			"--slice-dir=" + filepath.Join(t.TempDir(), "s"),
			"--snapshot-source=" + src, "--seek-from=1",
		}, "opposite directions"},
		{"slice-dir with snapshots", []string{
			"--slice-dir=" + filepath.Join(t.TempDir(), "s2"), "--snapshots",
		}, "already derives snapshots"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"run", "--boundary-log=" + src}, tc.args...)
			args = append(args, "testdata/boundary-log/balance_calc.wasm")
			code, _, stderr := runMain(t, "", args)
			require.Equal(t, 1, code)
			require.True(t, bytes.Contains([]byte(stderr), []byte(tc.wants)),
				"expected %q in:\n%s", tc.wants, stderr)
		})
	}
}
