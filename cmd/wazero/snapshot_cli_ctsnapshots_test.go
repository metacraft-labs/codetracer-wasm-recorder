//go:build ctsnapshots

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasmsnapshot"
)

// The commercial build's CLI surface, exercised against the committed browser
// recording — a real one, produced by the real pipeline (see
// `testdata/boundary-log/README.md`). Run with:
//
//	go test -tags ctsnapshots ./cmd/wazero/
//
// No mocks: the container, its snapshot namespaces and the seek all go through
// the shipped code paths.

// TestSnapshotsFlagWritesTheNamespacesIntoTheContainer covers the derivation
// half of `--snapshots`, including the "not sidecar files" rule of snapshot
// spec §6: the output directory gains no file, only the container grows.
func TestSnapshotsFlagWritesTheNamespacesIntoTheContainer(t *testing.T) {
	require.True(t, snapshotsAvailable)
	out := t.TempDir()
	exitCode, stdout, stderr := runMain(t, "", []string{
		"run",
		"--boundary-log=testdata/boundary-log/frontend-wasm.ct",
		"--snapshots",
		"--out-dir=" + out,
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 0, exitCode, "stderr:\n%s", stderr)
	require.True(t, bytes.Contains([]byte(stdout), []byte("attached")),
		"the run did not report attaching snapshots: %s", stdout)

	entries, err := os.ReadDir(out)
	require.NoError(t, err)
	require.Equal(t, 1, len(entries),
		"deriving snapshots wrote something beside the container: %v", entries)
	require.Equal(t, "balance_calc.ct", entries[0].Name())

	c, err := ctfs.Open(filepath.Join(out, "balance_calc.ct"))
	require.NoError(t, err)
	for _, name := range wasmsnapshot.NamespaceNames() {
		require.True(t, c.Has(name), "%s is missing from the container", name)
	}
	set, diag, err := wasmsnapshot.Load(filepath.Join(out, "balance_calc.ct"), false)
	require.NoError(t, err)
	require.Equal(t, "", diag)
	// The committed recording has one exported call, so two quiescent points.
	require.Equal(t, 2, len(set.Points()))
	require.Equal(t, 2, set.SnapshotCount())
	base, ok := set.Nearest(0)
	require.True(t, ok)
	require.True(t, base.IsBase(), "quiescent point 0 must be a slice base")
}

// TestSeekProducesTheSameTraceAsLinearReplay is the CLI-level form of the
// milestone's headline property. The seek distance is short — the committed
// recording has a single exported call — but the mechanism is the real one:
// the second run reconstructs the module's memory, globals and tables from the
// container's `snap*` namespaces instead of from instantiation, and its trace
// must be indistinguishable from the linear run's.
func TestSeekProducesTheSameTraceAsLinearReplay(t *testing.T) {
	require.True(t, snapshotsAvailable)
	dir := t.TempDir()

	withSnapshots := filepath.Join(dir, "derived")
	exitCode, _, stderr := runMain(t, "", []string{
		"run",
		"--boundary-log=testdata/boundary-log/frontend-wasm.ct",
		"--snapshots",
		"--out-dir=" + withSnapshots,
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 0, exitCode, "stderr:\n%s", stderr)
	source := filepath.Join(withSnapshots, "balance_calc.ct")

	linearDir := filepath.Join(dir, "linear")
	exitCode, _, stderr = runMain(t, "", []string{
		"run",
		"--boundary-log=testdata/boundary-log/frontend-wasm.ct",
		"--out-dir=" + linearDir,
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 0, exitCode, "stderr:\n%s", stderr)

	seekedDir := filepath.Join(dir, "seeked")
	exitCode, _, stderr = runMain(t, "", []string{
		"run",
		"--boundary-log=testdata/boundary-log/frontend-wasm.ct",
		"--snapshot-source=" + source,
		"--seek-from=0",
		"--out-dir=" + seekedDir,
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 0, exitCode, "stderr:\n%s", stderr)

	linear, err := ctfs.Open(filepath.Join(linearDir, "balance_calc.ct"))
	require.NoError(t, err)
	seeked, err := ctfs.Open(filepath.Join(seekedDir, "balance_calc.ct"))
	require.NoError(t, err)
	require.Equal(t, linear.Names(), seeked.Names())
	for _, name := range linear.Names() {
		a, err := linear.ReadFile(name)
		require.NoError(t, err)
		b, err := seeked.ReadFile(name)
		require.NoError(t, err)
		if name == "meta.dat" {
			// Only the container's own trace identifier may differ.
			require.Equal(t, len(a), len(b))
			continue
		}
		require.True(t, bytes.Equal(a, b),
			"stream %q differs between the linear and the snapshot-seeded run", name)
	}

	// And through the independent reader, when it is available.
	ctPrint := ctPrintPath(t)
	if _, err := os.Stat(ctPrint); err != nil {
		t.Logf("ct-print not found at %s — the cross-implementation check did not run", ctPrint)
		return
	}
	dump := func(p string) []byte {
		out, err := exec.Command(ctPrint, "--full", "--strip-paths", p).CombinedOutput()
		require.NoError(t, err, "ct-print failed:\n%s", out)
		return out
	}
	al := bytes.Split(dump(filepath.Join(linearDir, "balance_calc.ct")), []byte("\n"))
	bl := bytes.Split(dump(filepath.Join(seekedDir, "balance_calc.ct")), []byte("\n"))
	require.Equal(t, len(al), len(bl))
	for i := range al {
		if bytes.Equal(al[i], bl[i]) {
			continue
		}
		require.True(t, bytes.Contains(al[i], []byte("trace_id")),
			"ct-print line %d differs:\n linear: %s\n seeked: %s", i, al[i], bl[i])
	}
}

// TestSeekWithoutSnapshotsIsRefusedNotGuessed: asking to seek into a container
// that carries no snapshots must fail with an explanation, not silently fall
// back to a linear replay the user did not ask for and would not detect.
func TestSeekWithoutSnapshotsIsRefusedNotGuessed(t *testing.T) {
	dir := t.TempDir()
	plainDir := filepath.Join(dir, "plain")
	exitCode, _, stderr := runMain(t, "", []string{
		"run",
		"--boundary-log=testdata/boundary-log/frontend-wasm.ct",
		"--out-dir=" + plainDir,
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 0, exitCode, "stderr:\n%s", stderr)

	exitCode, _, stderr = runMain(t, "", []string{
		"run",
		"--boundary-log=testdata/boundary-log/frontend-wasm.ct",
		"--snapshot-source=" + filepath.Join(plainDir, "balance_calc.ct"),
		"--seek-from=1",
		"--out-dir=" + filepath.Join(dir, "seeked"),
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 1, exitCode)
	require.True(t, bytes.Contains([]byte(stderr), []byte("no usable snapshots")),
		"unhelpful refusal: %s", stderr)
	require.True(t, bytes.Contains([]byte(stderr), []byte("linear replay")),
		"the refusal does not name the alternative: %s", stderr)
}
