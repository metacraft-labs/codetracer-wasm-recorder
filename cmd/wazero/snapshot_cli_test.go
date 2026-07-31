//go:build !ctsnapshots

package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/boundarylog"
	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasmsnapshot"
)

// This file tests the OPEN build's obligations towards a snapshot-bearing
// `.ct` (`WASM-Replay-Snapshots-And-Slices.md` §9). It is deliberately
// untagged, so it compiles and runs in the default `just test` configuration —
// the one that produces the open artifact.
//
// It imports `internal/wasmsnapshot` in order to *produce* the container the
// open build must cope with. That import is in a `_test.go` file and therefore
// does not put the snapshot code into the `wazero` binary: `snapshots.go` (the
// only non-test file that imports it) carries `//go:build ctsnapshots`.
//
// No mocks: the container is produced by the real CLI through the real CTFS
// writer, the snapshot namespaces are real derived data, and the reader that
// has to tolerate them is `ct-print` from `codetracer-trace-format-nim` — an
// entirely separate implementation in a different language.

// materialiseBoundaryLog runs the CLI's `--boundary-log` path over the
// committed browser recording and returns the produced container's path.
func materialiseBoundaryLog(t *testing.T, outDir string) string {
	t.Helper()
	exitCode, _, stderr := runMain(t, "", []string{
		"run",
		"--boundary-log=testdata/boundary-log/frontend-wasm.ct",
		"--out-dir=" + outDir,
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 0, exitCode, "boundary-log replay failed; stderr:\n%s", stderr)
	return filepath.Join(outDir, "balance_calc.ct")
}

// attachSnapshotsTo derives snapshots for the committed recording and writes
// them into `containerPath`. This is what the commercial build's `--snapshots`
// does; the test does it directly so the open build under test is the thing
// being observed rather than the thing doing the work.
func attachSnapshotsTo(t *testing.T, containerPath string) {
	t.Helper()
	ctx := t.Context()
	bin, err := os.ReadFile("testdata/boundary-log/balance_calc.wasm")
	require.NoError(t, err)

	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer func() { _ = rt.Close(ctx) }()
	compiled, err := rt.CompileModule(ctx, bin)
	require.NoError(t, err)

	rec, err := boundarylog.LoadRecording("testdata/boundary-log/frontend-wasm.ct")
	require.NoError(t, err)
	points, err := wasmsnapshot.QuiescentPoints(rec)
	require.NoError(t, err)

	builder, err := wasmsnapshot.NewBuilder(points, wasmsnapshot.NewTiers(false),
		wasmsnapshot.EncodeOptions{})
	require.NoError(t, err)

	_, err = boundarylog.Replay(ctx, boundarylog.Options{
		Runtime: rt, Compiled: compiled, Recording: rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		AtQuiescentPoint: func(p int, mod api.Module) error {
			s, err := wasmsnapshot.Capture(mod, p)
			if err != nil {
				return err
			}
			return builder.Add(s)
		},
	})
	require.NoError(t, err)
	require.NoError(t, builder.AttachTo(containerPath))
}

// TestOpenBuildToleratesSnapshotNamespaces is
// `verify_open_build_reads_snapshot_bearing_container` at the artifact level.
//
// Two containers are produced from the same recording, one with the `wcp.*`
// namespaces attached and one without, and both are read back. The snapshot
// namespaces must change nothing about the trace the container holds.
//
// When `ct-print` is available the comparison is made through it as well —
// that is a reader written in a different language, by a different
// implementation of the CTFS format, and its indifference to the extra
// namespaces is the strongest available evidence that they are additive. Its
// absence weakens the test but does not hollow it out: the container-level
// comparison below runs unconditionally.
func TestOpenBuildToleratesSnapshotNamespaces(t *testing.T) {
	require.False(t, snapshotsAvailable,
		"this test asserts the OPEN build's behaviour; build without -tags ctsnapshots")

	dir := t.TempDir()
	plain := materialiseBoundaryLog(t, filepath.Join(dir, "plain"))
	bearing := materialiseBoundaryLog(t, filepath.Join(dir, "bearing"))
	attachSnapshotsTo(t, bearing)

	plainC, err := ctfs.Open(plain)
	require.NoError(t, err)
	bearingC, err := ctfs.Open(bearing)
	require.NoError(t, err)

	// Every trace stream is byte-identical; only the trace identifier in
	// meta.dat may differ, since it names the container rather than the
	// execution.
	for _, name := range plainC.Names() {
		a, err := plainC.ReadFile(name)
		require.NoError(t, err)
		b, err := bearingC.ReadFile(name)
		require.NoError(t, err)
		if name == "meta.dat" {
			require.Equal(t, len(a), len(b))
			continue
		}
		require.True(t, bytes.Equal(a, b),
			"stream %q changed once the snapshot namespaces were attached", name)
	}
	// And the container really does carry them.
	for _, name := range wasmsnapshot.NamespaceNames() {
		require.True(t, bearingC.Has(name), "%s is missing", name)
		require.False(t, plainC.Has(name), "%s should not be in the plain container", name)
	}

	// Cross-implementation check through ct-print.
	ctPrint := ctPrintPath(t)
	if _, err := os.Stat(ctPrint); err != nil {
		t.Logf("ct-print not found at %s — the cross-implementation half of this "+
			"test did not run; the container-level comparison above did", ctPrint)
		return
	}
	dump := func(path string) []byte {
		out, err := exec.Command(ctPrint, "--full", "--strip-paths", path).CombinedOutput()
		require.NoError(t, err, "ct-print failed on %s:\n%s", path, out)
		return out
	}
	a, b := dump(plain), dump(bearing)
	require.Equal(t, len(a), len(b),
		"ct-print produced different-length output for the snapshot-bearing container")
	// The two dumps carry different container trace ids; everything else must
	// match. Compare line by line so a real difference is legible.
	al, bl := bytes.Split(a, []byte("\n")), bytes.Split(b, []byte("\n"))
	require.Equal(t, len(al), len(bl))
	for i := range al {
		if bytes.Equal(al[i], bl[i]) {
			continue
		}
		require.True(t, bytes.Contains(al[i], []byte("trace_id")),
			"ct-print line %d differs and is not the container trace id:\n plain: %s\nbearing: %s",
			i, al[i], bl[i])
	}
}

// TestOpenBuildRefusesSnapshotFlagsWithAnExplanation: the open build knows the
// flags and says what it cannot do, rather than appearing not to know them.
func TestOpenBuildRefusesSnapshotFlagsWithAnExplanation(t *testing.T) {
	require.False(t, snapshotsAvailable)
	dir := t.TempDir()
	exitCode, _, stderr := runMain(t, "", []string{
		"run",
		"--boundary-log=testdata/boundary-log/frontend-wasm.ct",
		"--snapshots",
		"--out-dir=" + dir,
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 1, exitCode)
	require.True(t, bytes.Contains([]byte(stderr), []byte("does not derive or consume replay snapshots")),
		"unhelpful refusal: %s", stderr)
	// Nothing was written: a refused run must not leave a half-made trace.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Equal(t, 0, len(entries))
}

// TestOpenBuildRefusesSliceFlagsWithAnExplanation: slicing is snapshot
// derivation — every slice opens with a base snapshot, which is the only reason
// it is independently materialisable — so it falls on the commercial side of
// snapshot spec §9 and the open build says so rather than producing slices
// without bases.
func TestOpenBuildRefusesSliceFlagsWithAnExplanation(t *testing.T) {
	require.False(t, snapshotsAvailable)
	dir := t.TempDir()
	exitCode, _, stderr := runMain(t, "", []string{
		"run",
		"--boundary-log=testdata/boundary-log/frontend-wasm.ct",
		"--slice-dir=" + filepath.Join(dir, "slices"),
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 1, exitCode)
	require.True(t, bytes.Contains([]byte(stderr), []byte("does not split a recording into slices")),
		"unhelpful refusal: %s", stderr)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Equal(t, 0, len(entries))
}

// TestOpenBuildStreamsARecording: streaming is *not* commercial. It changes
// when a recording is read, not what is derived from it, and materialisation is
// open (snapshot spec §9). The open build must therefore accept
// `--boundary-stream` and produce the same trace the commercial one does.
func TestOpenBuildStreamsARecording(t *testing.T) {
	require.False(t, snapshotsAvailable)
	src := repeatRecording(t, 2)
	raw, err := os.ReadFile(filepath.Join(src, "trace.json"))
	require.NoError(t, err)

	live := filepath.Join(t.TempDir(), "frontend-wasm.ct")
	require.NoError(t, os.MkdirAll(live, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(live, "trace.json"), raw, 0o644))
	marker := filepath.Join(t.TempDir(), "done")
	require.NoError(t, os.WriteFile(marker, nil, 0o644))

	out := t.TempDir()
	exitCode, stdout, stderr := runMain(t, "", []string{
		"run",
		"--boundary-log=" + live,
		"--boundary-stream=" + filepath.Join(live, "trace.json"),
		"--stream-done=" + marker,
		"--out-dir=" + out,
		"testdata/boundary-log/balance_calc.wasm",
	})
	require.Equal(t, 0, exitCode, "stderr:\n%s", stderr)
	require.True(t, bytes.Contains([]byte(stdout), []byte("replayed 2 exported call(s)")), stdout)
	require.True(t, ctfsHas(t, filepath.Join(out, "balance_calc.ct"), "steps.dat"))
}
