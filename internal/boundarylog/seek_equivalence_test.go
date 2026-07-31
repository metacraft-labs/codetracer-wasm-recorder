// Package boundarylog_test holds the end-to-end tests for WASM replay
// snapshots: they need both `internal/boundarylog` (the replay driver) and
// `internal/wasmsnapshot` (the snapshot machinery), and the latter imports the
// former, so they cannot live in `package boundarylog`.
//
// NO MOCKS ARE USED HERE, and none would be defensible. Every assertion runs
// against the real committed `balance_calc.wasm`, the real wazero
// interpreter, the real CTFS trace writer, and real `.ct` containers on disk.
// The property under test is byte-level equality of two materialised traces;
// a stubbed writer or a fake module would test the stub, not the property.
package boundarylog_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/boundarylog"
	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/wasmsnapshot"
	"github.com/tetratelabs/wazero/internal/xxh3"
	"github.com/tetratelabs/wazero/tracewriter"
)

const balanceCalcWasm = "../../cmd/wazero/testdata/boundary-log/balance_calc.wasm"

// callArgs drives a five-call recording. The arguments differ per call so a
// mis-seek — replaying the wrong call, or replaying it against the wrong
// state — changes the trace's recorded values rather than merely its shape.
var callArgs = [][2]int32{{42, 100}, {7, 3}, {1000, 1}, {5, 5000}, {12, 34}}

// harness bundles everything one replay needs.
type harness struct {
	ctx      context.Context
	rt       wazero.Runtime
	compiled wazero.CompiledModule
	rec      *boundarylog.Recording
}

func newHarness(t *testing.T, ctDir string) *harness {
	t.Helper()
	ctx := context.Background()
	bin, err := os.ReadFile(balanceCalcWasm)
	require.NoError(t, err)

	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	t.Cleanup(func() { _ = rt.Close(ctx) })
	compiled, err := rt.CompileModule(ctx, bin)
	require.NoError(t, err)

	rec, err := boundarylog.LoadRecording(ctDir)
	require.NoError(t, err)
	return &harness{ctx: ctx, rt: rt, compiled: compiled, rec: rec}
}

// attachRecorderAt returns a quiescent-point hook that installs `w` as the
// module's trace recorder when the replay reaches `point`.
//
// This is how both sides of the equivalence test start recording at exactly
// the same place. `ModuleInstance.Record` is read live by the interpreter, so
// swapping it between exported calls is well defined — and a quiescent point
// is the only place it is, because the recorder's own call-depth counter must
// start from an empty WASM stack.
func attachRecorderAt(t *testing.T, point int, w tracewriter.TraceRecorder) func(int, api.Module) error {
	t.Helper()
	return func(p int, mod api.Module) error {
		if p != point {
			return nil
		}
		mi, ok := mod.(*wasm.ModuleInstance)
		if !ok {
			return fmt.Errorf("module is a %T, not a wazero ModuleInstance", mod)
		}
		mi.Record = w
		return nil
	}
}

// produce writes the buffered trace out as a real `.ct` container and returns
// its path.
func produce(t *testing.T, w tracewriter.TraceRecorder, dir string) string {
	t.Helper()
	return produceAs(t, w, dir, "balance_calc.wasm")
}

// produceAs is `produce` under an explicit program name.
//
// The name reaches `meta.dat`, so two containers that are otherwise identical
// differ in it — which is why a slice's own trace can only be compared against
// a linear materialisation produced under the *slice's* name.
func produceAs(t *testing.T, w tracewriter.TraceRecorder, dir, program string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, w.ProduceTrace(dir, program, "/fixed/workdir"))
	base := filepath.Base(program)
	return filepath.Join(dir, strings.TrimSuffix(base, filepath.Ext(base))+".ct")
}

// metaDatTraceID is the byte range of `meta.dat` holding the container's
// freshly generated UUIDv7 trace identifier: a `CTMD` magic (4) + version (2)
// + flags (2), then a length-prefixed 36-character UUID.
//
// It is the ONLY byte range in a `.ct` that differs between two runs of the
// identical trace — verified by `TestContainerBytesDifferOnlyInTheTraceID`
// below, which is what earns this test the right to mask it. The identifier
// names the container, not the execution, so two materialisations of the same
// range are expected to disagree about it.
const (
	metaDatIDLenOffset = 8
	metaDatIDLength    = 36
)

func maskTraceID(t *testing.T, meta []byte) []byte {
	t.Helper()
	if len(meta) < metaDatIDLenOffset+1+metaDatIDLength {
		t.Fatalf("meta.dat is only %d bytes; the trace-id field is not where expected", len(meta))
	}
	if got := meta[metaDatIDLenOffset]; got != metaDatIDLength {
		t.Fatalf("meta.dat byte %d is %d, expected the %d-byte trace-id length prefix",
			metaDatIDLenOffset, got, metaDatIDLength)
	}
	out := append([]byte(nil), meta...)
	for i := metaDatIDLenOffset + 1; i < metaDatIDLenOffset+1+metaDatIDLength; i++ {
		out[i] = '?'
	}
	return out
}

// compareContainers asserts two `.ct` files hold identical *trace* streams
// with identical bytes, modulo the per-container trace identifier.
//
// The `wcp.*` snapshot namespaces are excluded on purpose: they are additive
// derived data, present or absent independently of what the trace says, and
// the property under test is about the materialised trace. Excluding them is
// exactly the reading a snapshot-unaware reader takes.
func compareContainers(t *testing.T, wantPath, gotPath, wantLabel, gotLabel string) {
	t.Helper()
	want, err := ctfs.Open(wantPath)
	require.NoError(t, err)
	got, err := ctfs.Open(gotPath)
	require.NoError(t, err)

	wantNames, gotNames := traceStreams(want), traceStreams(got)
	require.Equal(t, wantNames, gotNames)

	for _, name := range wantNames {
		a, err := want.ReadFile(name)
		require.NoError(t, err)
		b, err := got.ReadFile(name)
		require.NoError(t, err)
		if name == "meta.dat" {
			a, b = maskTraceID(t, a), maskTraceID(t, b)
		}
		if !bytes.Equal(a, b) {
			t.Fatalf("stream %q differs between %s (%d bytes) and %s (%d bytes)",
				name, wantLabel, len(a), gotLabel, len(b))
		}
	}
}

// traceStreams lists a container's internal files with the snapshot
// namespaces removed.
func traceStreams(c *ctfs.Container) []string {
	snapshot := map[string]bool{}
	for _, n := range wasmsnapshot.NamespaceNames() {
		snapshot[n] = true
	}
	var out []string
	for _, n := range c.Names() {
		if !snapshot[n] {
			out = append(out, n)
		}
	}
	return out
}

// TestContainerBytesDifferOnlyInTheTraceID establishes the baseline the
// equivalence test rests on: materialising the *same* range twice produces
// containers that differ in nothing but the trace identifier. Without this,
// masking that field in `compareContainers` would be an unexamined loophole
// through which a real difference could slip.
func TestContainerBytesDifferOnlyInTheTraceID(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	dir := t.TempDir()

	var paths []string
	for i := 0; i < 2; i++ {
		h := newHarness(t, ctDir)
		w := tracewriter.NewCtfsTraceWriter()
		_, err := boundarylog.Replay(h.ctx, boundarylog.Options{
			Runtime:      h.rt,
			Compiled:     h.compiled,
			Recording:    h.rec,
			ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
			Recorder:     w,
		})
		require.NoError(t, err)
		paths = append(paths, produce(t, w, filepath.Join(dir, fmt.Sprintf("run%d", i))))
	}

	// Whole-file lengths first: a difference here would mean something other
	// than a fixed-width identifier changed.
	a, err := os.ReadFile(paths[0])
	require.NoError(t, err)
	b, err := os.ReadFile(paths[1])
	require.NoError(t, err)
	require.Equal(t, len(a), len(b))

	c0, err := ctfs.Open(paths[0])
	require.NoError(t, err)
	c1, err := ctfs.Open(paths[1])
	require.NoError(t, err)
	require.Equal(t, c0.Names(), c1.Names())

	for _, name := range c0.Names() {
		x, err := c0.ReadFile(name)
		require.NoError(t, err)
		y, err := c1.ReadFile(name)
		require.NoError(t, err)
		if name != "meta.dat" {
			if !bytes.Equal(x, y) {
				t.Fatalf("stream %q differs between two materialisations of the same "+
					"range; the container is not deterministic and the equivalence test "+
					"below would be meaningless", name)
			}
			continue
		}
		require.Equal(t, "CTMD", string(x[:4]))
		lo, hi := metaDatIDLenOffset+1, metaDatIDLenOffset+1+metaDatIDLength
		for i := range x {
			if x[i] == y[i] {
				continue
			}
			if i < lo || i >= hi {
				t.Fatalf("meta.dat differs at byte %d, outside the trace-id field "+
					"[%d,%d); masking that field would hide a real difference", i, lo, hi)
			}
		}
	}
}

// TestReadsAContainerWrittenByTheNimWriter pins `internal/ctfs` against the
// real producer.
//
// The `.ct` here is written by `codetracer-trace-format-nim`'s CTFS writer
// through the cgo FFI in `tracewriter/` — nothing in this repository shapes
// its bytes. The test asserts the container reader recovers the writer's own
// internal files with plausible, self-consistent content, which is what earns
// `dataBlocks`' claim that the Nim writer never applies the small-file
// optimisation: every one of these streams is well under one 4096-byte block
// and every one still resolves through a mapping block.
func TestReadsAContainerWrittenByTheNimWriter(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	h := newHarness(t, ctDir)
	w := tracewriter.NewCtfsTraceWriter()
	_, err := boundarylog.Replay(h.ctx, boundarylog.Options{
		Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(), Recorder: w,
	})
	require.NoError(t, err)
	container := produce(t, w, t.TempDir())

	c, err := ctfs.Open(container)
	require.NoError(t, err)
	for _, name := range []string{"meta.dat", "paths.dat", "steps.dat", "calls.dat", "values.dat"} {
		require.True(t, c.Has(name), "the Nim writer's %s is missing", name)
		b, err := c.ReadFile(name)
		require.NoError(t, err)
		require.True(t, len(b) > 0, "%s read back empty", name)
		require.True(t, uint64(len(b)) < c.BlockSize(),
			"%s is %d bytes; this test only demonstrates the sub-block case", name, len(b))
	}
	meta, err := c.ReadFile("meta.dat")
	require.NoError(t, err)
	require.Equal(t, "CTMD", string(meta[:4]))
	require.True(t, bytes.Contains(meta, []byte("balance_calc.wasm")),
		"meta.dat does not name the program; the reader is resolving the wrong blocks")
}

// deriveSnapshots replays the whole recording, capturing a snapshot at every
// quiescent point, and attaches the resulting `wcp.*` namespaces to
// `containerPath`. It returns the number of snapshots taken.
func deriveSnapshots(t *testing.T, ctDir, containerPath string, inline bool) int {
	t.Helper()
	return deriveSnapshotsFor(t, balanceCalcWasm, ctDir, containerPath, inline)
}

// deriveSnapshotsFor is `deriveSnapshots` for an arbitrary module, the way
// `newHarnessFor` is `newHarness` for one. It exists so the state-carrying
// fixture in `state_carrying_equivalence_test.go` derives its snapshots
// through exactly the same code path as `balance_calc` does, rather than
// through a second copy of it.
func deriveSnapshotsFor(
	t *testing.T, wasmPath, ctDir, containerPath string, inline bool,
) int {
	t.Helper()
	h := newHarnessFor(t, wasmPath, ctDir)
	points, err := wasmsnapshot.QuiescentPoints(h.rec)
	require.NoError(t, err)

	builder, err := wasmsnapshot.NewBuilder(points, wasmsnapshot.NewTiers(false),
		wasmsnapshot.EncodeOptions{InlineMissedPages: inline})
	require.NoError(t, err)

	_, err = boundarylog.Replay(h.ctx, boundarylog.Options{
		Runtime:      h.rt,
		Compiled:     h.compiled,
		Recording:    h.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		AtQuiescentPoint: func(point int, mod api.Module) error {
			snap, err := wasmsnapshot.Capture(mod, point)
			if err != nil {
				return err
			}
			return builder.Add(snap)
		},
	})
	require.NoError(t, err)
	require.NoError(t, builder.MarkBase(0))
	require.NoError(t, builder.AttachTo(containerPath))
	return builder.SnapshotCount()
}

// TestSnapshotMaterialisationIsByteIdentical is the milestone's headline
// property, and the reason snapshots may be treated as a pure optimisation:
//
//	verify_snapshot_materialisation_is_byte_identical —
//	"Materialising a sub-range via the nearest preceding snapshot produces a
//	 trace byte-identical to materialising the same range by linear replay
//	 from the start."
//
// Both sides materialise the range [k, N) of exported calls. They differ in
// exactly one thing: how the module reaches quiescent point k.
//
//   - LINEAR: instantiate, then replay calls 0..k with no recorder attached,
//     so the module arrives at point k by re-executing everything before it —
//     the slow path snapshots exist to avoid.
//   - SEEDED: instantiate, then restore the snapshot taken at point k.
//
// From point k on, the two runs are driven identically and record into a
// fresh CTFS writer each. The produced `.ct` containers are then compared
// stream by stream, byte for byte.
func TestSnapshotMaterialisationIsByteIdentical(t *testing.T) {
	for _, inline := range []bool{false, true} {
		name := "per-trace-cache"
		if inline {
			name = "inline-kind0-regions"
		}
		t.Run(name, func(t *testing.T) {
			ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
			work := t.TempDir()

			// A real container to attach the snapshot namespaces to: the
			// trace of the whole recording, produced the ordinary way.
			base := func() string {
				h := newHarness(t, ctDir)
				w := tracewriter.NewCtfsTraceWriter()
				_, err := boundarylog.Replay(h.ctx, boundarylog.Options{
					Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
					ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
					Recorder:     w,
				})
				require.NoError(t, err)
				return produce(t, w, filepath.Join(work, "full"))
			}()

			n := deriveSnapshots(t, ctDir, base, inline)
			require.Equal(t, len(callArgs)+1, n)

			set, diag, err := wasmsnapshot.Load(base, false)
			require.NoError(t, err)
			require.Equal(t, "", diag)
			require.NotNil(t, set)

			for k := 1; k < len(callArgs); k++ {
				t.Run(fmt.Sprintf("from-point-%d", k), func(t *testing.T) {
					// --- linear side ---------------------------------------
					linearW := tracewriter.NewCtfsTraceWriter()
					hl := newHarness(t, ctDir)
					_, err := boundarylog.Replay(hl.ctx, boundarylog.Options{
						Runtime: hl.rt, Compiled: hl.compiled, Recording: hl.rec,
						ModuleConfig:     wazero.NewModuleConfig().WithStartFunctions(),
						AtQuiescentPoint: attachRecorderAt(t, k, linearW),
					})
					require.NoError(t, err)
					linear := produce(t, linearW, filepath.Join(work,
						fmt.Sprintf("linear-%d", k)))

					// --- snapshot-seeded side ------------------------------
					rec, ok := set.Nearest(k)
					require.True(t, ok)
					require.Equal(t, uint32(k), rec.Ordinal)
					snap, err := set.Snapshot(rec)
					require.NoError(t, err)

					seededW := tracewriter.NewCtfsTraceWriter()
					hs := newHarness(t, ctDir)
					res, err := boundarylog.Replay(hs.ctx, boundarylog.Options{
						Runtime: hs.rt, Compiled: hs.compiled, Recording: hs.rec,
						ModuleConfig:     wazero.NewModuleConfig().WithStartFunctions(),
						FromPoint:        k,
						AtQuiescentPoint: attachRecorderAt(t, k, seededW),
						Resume:           func(mod api.Module) error { return snap.Restore(mod) },
					})
					require.NoError(t, err)
					require.Equal(t, k, res.FromPoint)
					require.Equal(t, len(callArgs)-k, res.ExportCalls)
					seeded := produce(t, seededW, filepath.Join(work,
						fmt.Sprintf("seeded-%d", k)))

					compareContainers(t, linear, seeded, "linear replay", "snapshot-seeded replay")
				})
			}
		})
	}
}

// TestBoundedRangeMaterialisationIsByteIdentical exercises the other end of a
// range: seeking to point k and stopping at point m, which is what
// materialising one *interval* of a slice does.
func TestBoundedRangeMaterialisationIsByteIdentical(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	work := t.TempDir()

	h0 := newHarness(t, ctDir)
	w0 := tracewriter.NewCtfsTraceWriter()
	_, err := boundarylog.Replay(h0.ctx, boundarylog.Options{
		Runtime: h0.rt, Compiled: h0.compiled, Recording: h0.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(), Recorder: w0,
	})
	require.NoError(t, err)
	base := produce(t, w0, filepath.Join(work, "full"))
	deriveSnapshots(t, ctDir, base, false)

	set, diag, err := wasmsnapshot.Load(base, false)
	require.NoError(t, err)
	require.Equal(t, "", diag)

	const from, to = 1, 3

	// Linear: reach `from` by re-execution, record until `to`, then stop
	// recording by detaching at `to`.
	linearW := tracewriter.NewCtfsTraceWriter()
	hl := newHarness(t, ctDir)
	_, err = boundarylog.Replay(hl.ctx, boundarylog.Options{
		Runtime: hl.rt, Compiled: hl.compiled, Recording: hl.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		AtQuiescentPoint: func(p int, mod api.Module) error {
			mi := mod.(*wasm.ModuleInstance)
			switch p {
			case from:
				mi.Record = linearW
			case to:
				mi.Record = nil
			}
			return nil
		},
	})
	require.NoError(t, err)
	linear := produce(t, linearW, filepath.Join(work, "linear"))

	rec, ok := set.Nearest(from)
	require.True(t, ok)
	snap, err := set.Snapshot(rec)
	require.NoError(t, err)

	seededW := tracewriter.NewCtfsTraceWriter()
	hs := newHarness(t, ctDir)
	res, err := boundarylog.Replay(hs.ctx, boundarylog.Options{
		Runtime: hs.rt, Compiled: hs.compiled, Recording: hs.rec,
		ModuleConfig:     wazero.NewModuleConfig().WithStartFunctions(),
		FromPoint:        from,
		ToPoint:          to,
		AtQuiescentPoint: attachRecorderAt(t, from, seededW),
		Resume:           func(mod api.Module) error { return snap.Restore(mod) },
	})
	require.NoError(t, err)
	require.Equal(t, to-from, res.ExportCalls)
	seeded := produce(t, seededW, filepath.Join(work, "seeded"))

	compareContainers(t, linear, seeded, "linear replay", "snapshot-seeded replay")
}

// TestSnapshotsAreNotSidecarFiles is
// `verify_snapshots_are_not_sidecar_files`: deriving snapshots must add
// nothing to the filesystem outside the `.ct` container itself.
func TestSnapshotsAreNotSidecarFiles(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	out := t.TempDir()

	h := newHarness(t, ctDir)
	w := tracewriter.NewCtfsTraceWriter()
	_, err := boundarylog.Replay(h.ctx, boundarylog.Options{
		Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(), Recorder: w,
	})
	require.NoError(t, err)
	container := produce(t, w, out)

	before := listTree(t, out)
	require.Equal(t, []string{"balance_calc.ct"}, before)

	deriveSnapshots(t, ctDir, container, false)

	after := listTree(t, out)
	require.Equal(t, before, after)

	// And the snapshot data really is reachable only through the container's
	// own namespaces.
	c, err := ctfs.Open(container)
	require.NoError(t, err)
	for _, name := range wasmsnapshot.NamespaceNames() {
		require.True(t, c.Has(name), name+" is missing from the container")
	}
}

func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		out = append(out, rel)
		return nil
	})
	require.NoError(t, err)
	return out
}

// TestOpenBuildReadsSnapshotBearingContainer is
// `verify_open_build_reads_snapshot_bearing_container`.
//
// The open build's whole obligation towards a snapshot-bearing container
// (snapshot spec §9) is to read it and ignore what it does not use. This test
// takes a container, attaches snapshot namespaces to it, and then asserts that
// every stream the recording is actually made of comes back byte-identical to
// what it was before — i.e. the namespaces are purely additive, and a reader
// that only knows the boundary streams sees an unchanged, complete trace.
func TestOpenBuildReadsSnapshotBearingContainer(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	work := t.TempDir()

	h := newHarness(t, ctDir)
	w := tracewriter.NewCtfsTraceWriter()
	_, err := boundarylog.Replay(h.ctx, boundarylog.Options{
		Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(), Recorder: w,
	})
	require.NoError(t, err)
	container := produce(t, w, work)

	pre, err := ctfs.Open(container)
	require.NoError(t, err)
	original := map[string][]byte{}
	for _, name := range pre.Names() {
		b, err := pre.ReadFile(name)
		require.NoError(t, err)
		original[name] = b
	}

	deriveSnapshots(t, ctDir, container, false)

	post, err := ctfs.Open(container)
	require.NoError(t, err)
	for name, want := range original {
		got, err := post.ReadFile(name)
		require.NoError(t, err)
		if !bytes.Equal(got, want) {
			t.Errorf("stream %q changed when the snapshot namespaces were attached", name)
		}
	}
	// The container gained exactly the six snapshot namespaces and nothing
	// else.
	require.Equal(t, len(original)+len(wasmsnapshot.NamespaceNames()), len(post.Names()))
}

// TestUnknownSnapshotVersionDegradesToLinearReplay is
// `verify_unknown_snapshot_version_degrades_to_linear_replay`.
//
// This is the deliberate narrowing of the MCR rule (snapshot spec §6, last
// paragraph): an unrecognised snapshot version disables seeking and NOTHING
// else. A reader must fall back to linear replay and say so, not reject a
// recording it can perfectly well materialise.
func TestUnknownSnapshotVersionDegradesToLinearReplay(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	work := t.TempDir()

	h := newHarness(t, ctDir)
	w := tracewriter.NewCtfsTraceWriter()
	_, err := boundarylog.Replay(h.ctx, boundarylog.Options{
		Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(), Recorder: w,
	})
	require.NoError(t, err)
	container := produce(t, w, work)

	// Build the snapshot streams, then bump `wcp.idx`'s version field to
	// something this build does not know before attaching them. Everything
	// else about the container is untouched.
	future := futureVersionStreams(t, ctDir)
	c, err := ctfs.Open(container)
	require.NoError(t, err)
	require.NoError(t, c.AddFiles(future))

	set, diag, err := wasmsnapshot.Load(container, false)
	require.NoError(t, err) // NOT an error: that is the whole point.
	require.Nil(t, set)
	require.True(t, len(diag) > 0, "an unreadable snapshot version must produce a diagnostic")
	require.True(t, bytes.Contains([]byte(diag), []byte("linear replay")),
		"the diagnostic must say what happens instead; got %q", diag)

	// And the recording still materialises, linearly, to the same trace.
	h2 := newHarness(t, ctDir)
	w2 := tracewriter.NewCtfsTraceWriter()
	_, err = boundarylog.Replay(h2.ctx, boundarylog.Options{
		Runtime: h2.rt, Compiled: h2.compiled, Recording: h2.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(), Recorder: w2,
	})
	require.NoError(t, err)
	again := produce(t, w2, filepath.Join(work, "again"))
	compareContainers(t, again, container,
		"linear replay of a plain container", "linear replay of the future-version container")
}

// TestReturnValueChecksumsCatchDivergenceInASeekedRange is
// `verify_return_value_checksums_catch_divergence`, in the form snapshot spec
// §10 gives it: "Every quiescent point therefore carries a free check on the
// replay that produced the snapshot preceding it; a mismatch is a divergence
// and a hard error."
//
// `TestDivergentExportReturnValueIsAHardError` already covers the linear case.
// What matters here is that seeking does not lose the check: a replay that
// starts in the middle of a recording must still compare every exported return
// value it produces against the recording, or a snapshot-seeded materialisation
// could silently be a fabrication where a linear one would have failed.
func TestReturnValueChecksumsCatchDivergenceInASeekedRange(t *testing.T) {
	// Corrupt the recorded result of the *fourth* call only, so the range
	// [1,5) contains it and the snapshot at point 1 is unaffected.
	corrupt := append([][2]int32(nil), callArgs...)
	honest := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	work := t.TempDir()

	h0 := newHarness(t, honest)
	w0 := tracewriter.NewCtfsTraceWriter()
	_, err := boundarylog.Replay(h0.ctx, boundarylog.Options{
		Runtime: h0.rt, Compiled: h0.compiled, Recording: h0.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(), Recorder: w0,
	})
	require.NoError(t, err)
	base := produce(t, w0, work)
	deriveSnapshots(t, honest, base, false)

	set, _, err := wasmsnapshot.Load(base, false)
	require.NoError(t, err)
	rec, ok := set.Nearest(1)
	require.True(t, ok)
	snap, err := set.Snapshot(rec)
	require.NoError(t, err)

	// Now build a recording whose fourth call claims a result the module will
	// not produce. Everything else about it is honest.
	tampered := boundarylog.BuildTamperedComputeBalanceRecording(t, t.TempDir(), corrupt, 3, 999999)
	ht := newHarness(t, tampered)
	_, err = boundarylog.Replay(ht.ctx, boundarylog.Options{
		Runtime: ht.rt, Compiled: ht.compiled, Recording: ht.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		FromPoint:    1,
		Resume:       func(mod api.Module) error { return snap.Restore(mod) },
	})
	require.Error(t, err)
	var div *boundarylog.DivergenceError
	require.True(t, errorsAs(err, &div),
		"a tampered return value inside a seeked range produced %T: %v", err, err)
	require.True(t, bytes.Contains([]byte(div.What), []byte("exported return value")),
		"the divergence names %q", div.What)
	require.Equal(t, "i32:999999", div.Recorded)
}

// errorsAs is `errors.As`, spelled out so this file needs no extra import
// alias next to the many it already carries.
func errorsAs(err error, target **boundarylog.DivergenceError) bool {
	for err != nil {
		if d, ok := err.(*boundarylog.DivergenceError); ok {
			*target = d
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// futureVersionStreams derives a real snapshot set and rewrites its `wcp.idx`
// version field to `SnapshotFormatVersion + 1`.
func futureVersionStreams(t *testing.T, ctDir string) map[string][]byte {
	t.Helper()
	h := newHarness(t, ctDir)
	points, err := wasmsnapshot.QuiescentPoints(h.rec)
	require.NoError(t, err)
	builder, err := wasmsnapshot.NewBuilder(points, wasmsnapshot.NewTiers(false),
		wasmsnapshot.EncodeOptions{})
	require.NoError(t, err)
	_, err = boundarylog.Replay(h.ctx, boundarylog.Options{
		Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		AtQuiescentPoint: func(point int, mod api.Module) error {
			s, err := wasmsnapshot.Capture(mod, point)
			if err != nil {
				return err
			}
			return builder.Add(s)
		},
	})
	require.NoError(t, err)

	streams := builder.Streams()
	idx := streams[wasmsnapshot.NamespaceIndex]
	require.True(t, len(idx) > 6)
	// Version is a u16 LE at offset 4 of the `WCPI` header.
	idx[4] = byte(wasmsnapshot.SnapshotFormatVersion + 1)
	idx[5] = 0
	return streams
}

// TestSnapshotRestoreReproducesTheLinearStateExactly is the sensitivity floor
// under TestSnapshotMaterialisationIsByteIdentical, and it exists because that
// test on its own does not have one.
//
// The committed fixture's only export, `compute_balance(user_id, amount)`, is
// a **pure function of its arguments** (see `BuildComputeBalanceRecording`).
// Its trace therefore does not depend on the linear memory the call starts
// from, so the byte-identity property above is satisfied by any resume at all
// — including one that restores nothing. Verified by mutation: deleting the
// memory copy from `Snapshot.Restore`, or the global write, leaves
// TestSnapshotMaterialisationIsByteIdentical green.
//
// That does not make the byte-identity test worthless — it is the property the
// spec §10 asks for, and it does prove the seeded and linear paths converge on
// the same *trace*. But the claim snapshots actually rest on is stronger:
// restoring a snapshot must put the module in exactly the state linear replay
// would have reached. This test asserts that directly, byte for byte, over
// state that demonstrably differs between points (the fixture's memory hash
// changes at every one), so a restore that dropped memory, globals or tables
// fails here even when the materialised trace cannot tell.
func TestSnapshotRestoreReproducesTheLinearStateExactly(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	work := t.TempDir()
	base := func() string {
		h := newHarness(t, ctDir)
		w := tracewriter.NewCtfsTraceWriter()
		_, err := boundarylog.Replay(h.ctx, boundarylog.Options{
			Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
			ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(), Recorder: w,
		})
		require.NoError(t, err)
		return produce(t, w, filepath.Join(work, "full"))
	}()
	deriveSnapshots(t, ctDir, base, false)

	set, diag, err := wasmsnapshot.Load(base, false)
	require.NoError(t, err)
	require.Equal(t, "", diag)

	// captureAt runs a replay and returns the state observed at `point`.
	captureAt := func(point int, opts boundarylog.Options) *wasmsnapshot.Snapshot {
		t.Helper()
		var got *wasmsnapshot.Snapshot
		h := newHarness(t, ctDir)
		opts.Runtime, opts.Compiled, opts.Recording = h.rt, h.compiled, h.rec
		opts.ModuleConfig = wazero.NewModuleConfig().WithStartFunctions()
		opts.AtQuiescentPoint = func(p int, mod api.Module) error {
			if p != point {
				return nil
			}
			s, err := wasmsnapshot.Capture(mod, p)
			if err != nil {
				return err
			}
			got = s
			return nil
		}
		_, err := boundarylog.Replay(h.ctx, opts)
		require.NoError(t, err)
		require.NotNil(t, got)
		return got
	}

	// The state must actually differ between points, or this test would pass
	// vacuously in exactly the way the byte-identity test does.
	var seen []xxh3.Uint128
	for k := 0; k <= len(callArgs); k++ {
		seen = append(seen, xxh3.Hash128(captureAt(k, boundarylog.Options{}).Memory))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] == seen[i-1] {
			t.Fatalf("the module's memory is identical at quiescent points %d and %d; "+
				"this fixture cannot distinguish a faithful restore from none", i-1, i)
		}
	}

	for k := 1; k < len(callArgs); k++ {
		t.Run(fmt.Sprintf("point-%d", k), func(t *testing.T) {
			linear := captureAt(k, boundarylog.Options{})

			rec, ok := set.Nearest(k)
			require.True(t, ok)
			require.Equal(t, uint32(k), rec.Ordinal)
			snap, err := set.Snapshot(rec)
			require.NoError(t, err)
			seeded := captureAt(k, boundarylog.Options{
				FromPoint: k,
				Resume:    func(mod api.Module) error { return snap.Restore(mod) },
			})

			require.Equal(t, linear.MemoryBytes, seeded.MemoryBytes)
			if !bytes.Equal(linear.Memory, seeded.Memory) {
				diffs, first := 0, -1
				for i := range linear.Memory {
					if linear.Memory[i] != seeded.Memory[i] {
						if first < 0 {
							first = i
						}
						diffs++
					}
				}
				t.Fatalf("restored memory differs from linear replay in %d byte(s), "+
					"first at %d (page %d)", diffs, first, first/wasmsnapshot.PageSize)
			}
			require.Equal(t, linear.Globals, seeded.Globals)
			require.Equal(t, linear.Tables, seeded.Tables)
		})
	}
}

// TestRestoreRewritesGlobalsAndTables covers what the test above cannot.
//
// The fixture module never changes a global or a table entry — every quiescent
// point has the same three globals and the same one-element table — so
// comparing the linear and restored states says nothing about whether Restore
// writes them at all. (Verified by mutation: deleting the global write, or the
// table write, from `Snapshot.Restore` leaves the comparison green.)
//
// So this test creates the difference the fixture does not: it scribbles over
// the live module's globals and table references and then restores, which fails
// unless Restore really rewrites both. Snapshot spec §4 lists globals and
// tables alongside linear memory; a restore that silently skipped them would
// be wrong for any module that used them, and this repository has no such
// module committed to notice with.
func TestRestoreRewritesGlobalsAndTables(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)

	var captured *wasmsnapshot.Snapshot
	var restored *wasmsnapshot.Snapshot
	h := newHarness(t, ctDir)
	_, err := boundarylog.Replay(h.ctx, boundarylog.Options{
		Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		AtQuiescentPoint: func(p int, mod api.Module) error {
			if p != 1 {
				return nil
			}
			s, err := wasmsnapshot.Capture(mod, p)
			if err != nil {
				return err
			}
			captured = s

			// Scribble: every global gets a distinct wrong value and every
			// table slot is nulled.
			mi := mod.(*wasm.ModuleInstance)
			require.True(t, len(mi.Globals) > 0, "the fixture has no globals to test with")
			for i, g := range mi.Globals {
				g.Val ^= uint64(0xa5a5a5a5 + i)
				g.ValHi ^= 0x5a5a5a5a
			}
			tableSlots := 0
			for _, tbl := range mi.Tables {
				for j := range tbl.References {
					tbl.References[j] = 0
					tableSlots++
				}
			}
			require.True(t, tableSlots > 0, "the fixture has no table slots to test with")

			if err := s.Restore(mod); err != nil {
				return err
			}
			restored, err = wasmsnapshot.Capture(mod, p)
			return err
		},
	})
	require.NoError(t, err)
	require.NotNil(t, captured)
	require.NotNil(t, restored)
	require.Equal(t, captured.Globals, restored.Globals)
	require.Equal(t, captured.Tables, restored.Tables)
}
