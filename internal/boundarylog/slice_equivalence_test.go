// Slice production and per-slice materialisation —
// `WASM-Replay-Snapshots-And-Slices.md` §7.
//
// NO MOCKS ARE USED HERE, and none would be defensible. Slices are produced by
// a real replay of a real committed `.wasm` through the real wazero
// interpreter, written by the real CTFS trace writer into real `.ct`
// containers, and materialised back out of those containers by a second real
// replay. The property under test — "slice N alone materialises its range" —
// is a property of bytes on disk; a fake container or a stub writer would test
// the fake.
//
// Two fixtures are used, and the split is deliberate:
//
//   - `balance_calc.wasm` carries DWARF, so its replay produces real
//     step/call/value streams. It is what the byte-identity comparisons run
//     over: without trace content there would be nothing to compare.
//   - `grow_mem.wasm` carries **state across exported calls** — every one of
//     its exports returns a function of accumulated state, not of its
//     arguments. It is what gives the independence claim teeth: a slice
//     materialised without its base snapshot restored diverges at its first
//     call. `balance_calc` cannot show that, because its only export is pure
//     (see the M38 review note in `seek_equivalence_test.go`).
package boundarylog_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/boundarylog"
	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/wasmsnapshot"
	"github.com/tetratelabs/wazero/tracewriter"
)

const growMemWasm = "../wasmsnapshot/testdata/grow_mem.wasm"

// growMemCalls drives a twelve-call recording that interleaves all three
// exports. Six `bump`s grow the memory from one page to seven, inside the
// module's declared maximum of eight.
var growMemCalls = []boundarylog.GrowMemCall{
	{Fn: "bump", Arg: 0x11}, {Fn: "size"}, {Fn: "bump", Arg: 0x22}, {Fn: "calls"},
	{Fn: "bump", Arg: 0x33}, {Fn: "size"}, {Fn: "bump", Arg: 0x44}, {Fn: "calls"},
	{Fn: "bump", Arg: 0x55}, {Fn: "size"}, {Fn: "bump", Arg: 0x66}, {Fn: "calls"},
}

// newHarnessFor is `newHarness` for an arbitrary module.
func newHarnessFor(t *testing.T, wasmPath, ctDir string) *harness {
	t.Helper()
	ctx := context.Background()
	bin, err := os.ReadFile(wasmPath)
	require.NoError(t, err)

	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	t.Cleanup(func() { _ = rt.Close(ctx) })
	compiled, err := rt.CompileModule(ctx, bin)
	require.NoError(t, err)

	rec, err := boundarylog.LoadRecording(ctDir)
	require.NoError(t, err)
	return &harness{ctx: ctx, rt: rt, compiled: compiled, rec: rec}
}

// sliceRun bundles what one slicing replay produced.
type sliceRun struct {
	dir      string
	manifest *wasmsnapshot.SliceManifest
	// sealedDuring records, for each slice, the quiescent point the replay had
	// reached when that slice's container appeared on disk. It is how the
	// "slices are emitted *during* the recording" claim is checked rather than
	// asserted.
	sealedDuring []int
}

// produceSlices replays `ctDir` against `wasmPath`, splitting the trace into
// slice containers under a fresh directory.
func produceSlices(
	t *testing.T, wasmPath, ctDir, program string, policy wasmsnapshot.SlicePolicy,
) sliceRun {
	t.Helper()
	h := newHarnessFor(t, wasmPath, ctDir)
	dir := filepath.Join(t.TempDir(), "slices")

	run := sliceRun{dir: dir}
	current := -1
	sw, err := wasmsnapshot.NewSliceWriter(wasmsnapshot.SliceOptions{
		Dir:         dir,
		Program:     program,
		Workdir:     "/fixed/workdir",
		Policy:      policy,
		NewRecorder: func() tracewriter.TraceRecorder { return tracewriter.NewCtfsTraceWriter() },
		OnSealed: func(info wasmsnapshot.SliceInfo) error {
			// The container must already be readable at the moment the replay
			// hands it over — that is what "uploaded as it becomes ready"
			// means for a producer with no uploader attached.
			if _, err := os.Stat(filepath.Join(dir, info.File)); err != nil {
				return fmt.Errorf("slice %d was handed over before it existed: %w", info.Index, err)
			}
			run.sealedDuring = append(run.sealedDuring, current)
			return nil
		},
	})
	require.NoError(t, err)

	_, err = boundarylog.Replay(h.ctx, boundarylog.Options{
		Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		// No Recorder: the slice writer attaches one per slice, and
		// `ModuleInstance.Record` holds exactly one.
		AtQuiescentPoint: func(p int, mod api.Module) error {
			current = p
			return sw.AtQuiescentPoint(p, mod)
		},
	})
	require.NoError(t, err)

	m, path, err := sw.Finish()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, wasmsnapshot.SliceManifestName), path)
	run.manifest = m
	return run
}

// isolate copies exactly one slice container into a directory of its own and
// returns its path.
//
// This is the whole point of the exercise: everything the materialisation
// below does must be doable with this one file. If it reached for a sibling
// slice, or for the whole-recording container, it would fail to open it.
func isolate(t *testing.T, run sliceRun, index int) string {
	t.Helper()
	info := run.manifest.Slices[index]
	raw, err := os.ReadFile(filepath.Join(run.dir, info.File))
	require.NoError(t, err)
	dst := filepath.Join(t.TempDir(), "alone", info.File)
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o755))
	require.NoError(t, os.WriteFile(dst, raw, 0o644))

	entries, err := os.ReadDir(filepath.Dir(dst))
	require.NoError(t, err)
	require.Equal(t, 1, len(entries))
	return dst
}

// materialiseFromSlice replays the range a slice describes, reaching its base
// quiescent point through the slice's own base snapshot, and returns the
// produced `.ct`.
//
// The range is read out of the slice container itself (`Set.Range`), not from
// the manifest: a slice must be self-describing, or "fetch slice N alone" would
// really be "fetch slice N and the manifest".
func materialiseFromSlice(t *testing.T, wasmPath, ctDir, slicePath, outDir string) (string, int, int) {
	t.Helper()
	// `useSystemCache: false` is load-bearing. It means the only place a memory
	// page may come from is this container's own `wcppages.ns`, so a slice that
	// referenced pages introduced by an earlier slice fails here rather than
	// silently resolving against a warm host cache.
	set, diag, err := wasmsnapshot.Load(slicePath, false)
	require.NoError(t, err)
	require.Equal(t, "", diag)
	require.NotNil(t, set)

	base, end, hasBase := set.Range()
	require.True(t, hasBase, "slice %s does not open with a base snapshot", slicePath)

	rec, ok := set.Nearest(base)
	require.True(t, ok)
	require.Equal(t, uint32(base), rec.Ordinal)
	require.True(t, rec.IsBase())
	snap, err := set.Snapshot(rec)
	require.NoError(t, err)

	w := tracewriter.NewCtfsTraceWriter()
	h := newHarnessFor(t, wasmPath, ctDir)
	res, err := boundarylog.Replay(h.ctx, boundarylog.Options{
		Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
		ModuleConfig:     wazero.NewModuleConfig().WithStartFunctions(),
		FromPoint:        base,
		ToPoint:          end,
		AtQuiescentPoint: attachRecorderAt(t, base, w),
		Resume:           func(mod api.Module) error { return snap.Restore(mod) },
	})
	require.NoError(t, err)
	require.Equal(t, end-base, res.ExportCalls)
	return produce(t, w, outDir), base, end
}

// materialiseLinearRange materialises [from,to) the slow way: instantiate, run
// every call from the beginning, record only inside the range.
//
// `program` is the name the container is produced under, because it reaches
// `meta.dat` and therefore has to match whatever the container being compared
// against was produced with.
func materialiseLinearRange(
	t *testing.T, wasmPath, ctDir string, from, to int, outDir, program string,
) string {
	t.Helper()
	w := tracewriter.NewCtfsTraceWriter()
	h := newHarnessFor(t, wasmPath, ctDir)
	_, err := boundarylog.Replay(h.ctx, boundarylog.Options{
		Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		AtQuiescentPoint: func(p int, mod api.Module) error {
			mi, ok := mod.(*wasm.ModuleInstance)
			if !ok {
				return fmt.Errorf("module is a %T, not a wazero ModuleInstance", mod)
			}
			switch p {
			case from:
				mi.Record = w
			case to:
				mi.Record = nil
			}
			return nil
		},
	})
	require.NoError(t, err)
	return produceAs(t, w, outDir, program)
}

// ---------------------------------------------------------------------------
// Production
// ---------------------------------------------------------------------------

// TestSlicesTileTheRecordingAndAreSealedDuringTheReplay covers the production
// half of snapshot spec §7's timeline: slices are sealed at quiescent points as
// the replay reaches them, not gathered at the end, and their ranges partition
// the recording exactly.
func TestSlicesTileTheRecordingAndAreSealedDuringTheReplay(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	run := produceSlices(t, balanceCalcWasm, ctDir, "balance_calc.wasm",
		wasmsnapshot.SlicePolicy{EveryPoints: 2})

	// Five calls, sealing every two quiescent points: [0,2), [2,4), [4,5).
	require.Equal(t, 3, len(run.manifest.Slices))
	require.Equal(t, len(callArgs)+1, run.manifest.TotalPoints)

	want := [][2]int{{0, 2}, {2, 4}, {4, 5}}
	for i, s := range run.manifest.Slices {
		require.Equal(t, i, s.Index)
		require.Equal(t, want[i][0], s.BasePoint)
		require.Equal(t, want[i][1], s.EndPoint)
		require.Equal(t, want[i][1]-want[i][0], s.ExportCalls)
		require.True(t, s.Bytes > 0, "slice %d is empty on disk", i)
		require.True(t, s.Snapshots >= 1, "slice %d carries no base snapshot", i)
	}

	// Sealed *during* the replay: `sealedDuring[i]` is the quiescent point the
	// replay had reached when slice i was handed over. The first two were
	// handed over while the replay still had calls left to drive; the last is
	// sealed by `Finish`, the "page unloads" step of the §7 timeline.
	require.Equal(t, []int{2, 4, 5}, run.sealedDuring)
	for i := 0; i < len(run.sealedDuring)-1; i++ {
		require.True(t, run.sealedDuring[i] < len(callArgs),
			"slice %d was not handed over until the recording was over", i)
	}
}

// TestSlicingIsAbsentByDefault: "possibly absent entirely" (snapshot spec §7)
// has to stay a real configuration, not a degenerate one.
func TestSlicingIsAbsentByDefault(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	out := t.TempDir()

	h := newHarness(t, ctDir)
	w := tracewriter.NewCtfsTraceWriter()
	_, err := boundarylog.Replay(h.ctx, boundarylog.Options{
		Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(), Recorder: w,
	})
	require.NoError(t, err)
	produce(t, w, out)
	require.Equal(t, []string{"balance_calc.ct"}, listTree(t, out))
}

// TestZeroPolicyProducesOneSlice: the zero `SlicePolicy` seals nothing early,
// so the recording becomes a single slice — MCR's `--no-split`.
func TestZeroPolicyProducesOneSlice(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	run := produceSlices(t, balanceCalcWasm, ctDir, "balance_calc.wasm",
		wasmsnapshot.SlicePolicy{})
	require.Equal(t, 1, len(run.manifest.Slices))
	require.Equal(t, 0, run.manifest.Slices[0].BasePoint)
	require.Equal(t, len(callArgs), run.manifest.Slices[0].EndPoint)
	// The one slice is handed over by `Finish`, at the last quiescent point.
	require.Equal(t, []int{len(callArgs)}, run.sealedDuring)
}

// TestByteTargetSealsSlices exercises the size trigger, which is the knob
// snapshot spec §7's "typically 1–50 MB" refers to. `grow_mem` is used because
// its memory grows a page per call, so the snapshot payload crosses any
// threshold in a predictable number of points; `balance_calc`'s memory never
// changes size and dedups to almost nothing.
func TestByteTargetSealsSlices(t *testing.T) {
	ctDir := boundarylog.BuildGrowMemRecording(t, t.TempDir(), growMemCalls)
	// Three 64 KiB pages' worth of payload.
	run := produceSlices(t, growMemWasm, ctDir, "grow_mem.wasm",
		wasmsnapshot.SlicePolicy{TargetBytes: 3 * wasmsnapshot.PageSize})

	require.True(t, len(run.manifest.Slices) > 1,
		"a byte target of %d produced a single slice", 3*wasmsnapshot.PageSize)
	for i, s := range run.manifest.Slices {
		require.True(t, s.EndPoint > s.BasePoint, "slice %d covers no call", i)
	}
	// And it tiles, which `Finish` also enforces.
	require.Equal(t, 0, run.manifest.Slices[0].BasePoint)
	require.Equal(t, len(growMemCalls),
		run.manifest.Slices[len(run.manifest.Slices)-1].EndPoint)
}

// ---------------------------------------------------------------------------
// Independent materialisation
// ---------------------------------------------------------------------------

// TestSliceIsIndependentlyMaterialisable is
// `verify_slice_is_independently_materialisable`, over the DWARF-bearing
// fixture so there is a real trace to compare.
//
// Each slice is copied into a directory of its own — no manifest, no siblings,
// no whole-recording container — and materialised from nothing but that file
// plus the two inputs every consumer has anyway (the module and the boundary
// log). The result is compared byte for byte against materialising the same
// range by linear replay from the start.
func TestSliceIsIndependentlyMaterialisable(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	run := produceSlices(t, balanceCalcWasm, ctDir, "balance_calc.wasm",
		wasmsnapshot.SlicePolicy{EveryPoints: 2})
	work := t.TempDir()

	for i := range run.manifest.Slices {
		t.Run(fmt.Sprintf("slice-%d", i), func(t *testing.T) {
			alone := isolate(t, run, i)
			got, base, end := materialiseFromSlice(t, balanceCalcWasm, ctDir, alone,
				filepath.Join(work, fmt.Sprintf("from-slice-%d", i)))
			require.Equal(t, run.manifest.Slices[i].BasePoint, base)
			require.Equal(t, run.manifest.Slices[i].EndPoint, end)

			want := materialiseLinearRange(t, balanceCalcWasm, ctDir, base, end,
				filepath.Join(work, fmt.Sprintf("linear-%d", i)), "balance_calc.wasm")
			compareContainers(t, want, got,
				"linear replay of the same range", "materialisation from slice alone")
		})
	}
}

// TestSliceTraceEqualsTheLinearTraceOfItsRange closes the other half of the
// claim: the trace a slice **carries** — the bytes recorded into it while it
// was being produced — is the trace of its range, not merely a trace that can
// be regenerated from it.
//
// Together with `TestSlicesTileTheRecordingAndAreSealedDuringTheReplay`'s
// tiling assertion, this is what "a trace assembled from slices equals the
// trace materialised linearly" reduces to for a set of containers: every
// slice's own trace streams equal the linear materialisation of its range, and
// the ranges partition the recording with no gap and no overlap.
//
// It is also the regression pin for `ModuleInstance.SetRecorder`. Producing a
// recording as slices swaps the recorder on one *live* module instance, and the
// instance carries the memo of which DWARF types the recorder has already been
// told about. Written as a bare `mi.Record = w`, slices after the first never
// received the type registrations the first slice consumed, and their
// `types.dat` came out holding the wrong type — measured, before the fix, as
// slice 1 and 2 declaring `i64` where linear replay declared `u32`, with every
// other stream byte-identical. Reverting `SetRecorder` to a field write fails
// this test and nothing else in the suite.
func TestSliceTraceEqualsTheLinearTraceOfItsRange(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	run := produceSlices(t, balanceCalcWasm, ctDir, "balance_calc.wasm",
		wasmsnapshot.SlicePolicy{EveryPoints: 2})
	work := t.TempDir()

	for i, s := range run.manifest.Slices {
		t.Run(fmt.Sprintf("slice-%d", i), func(t *testing.T) {
			// Produced under the slice's own program name, because that name
			// reaches `meta.dat` and the slice carries the name it was sealed
			// with.
			want := materialiseLinearRange(t, balanceCalcWasm, ctDir,
				s.BasePoint, s.EndPoint, filepath.Join(work, fmt.Sprintf("linear-%d", i)),
				wasmsnapshot.SliceProgramName("balance_calc.wasm", s.Index))
			compareContainers(t, want, filepath.Join(run.dir, s.File),
				"linear replay of the range", "the slice as produced")
		})
	}
}

// TestSliceMaterialisationNeedsItsBaseSnapshot is the sensitivity floor under
// every test above, and it is why this file carries a second fixture.
//
// `balance_calc`'s only export is a pure function of its arguments, so its
// slices materialise correctly even from a resume that restores nothing —
// exactly the blind spot the M38 review found. `grow_mem` has no such blind
// spot: `bump` returns the page count *before* it grew, `size` returns the
// current page count and `calls` returns a global counter, so the first call of
// slice N cannot produce its recorded result unless slice N's base snapshot put
// the module in the state linear replay reaches at that point.
//
// The test therefore asserts both directions:
//
//  1. materialising slice N alone, from its base snapshot, succeeds and lands
//     the module in exactly the state linear replay reaches at the slice's end;
//  2. materialising the same range **without** restoring the base snapshot
//     fails with a divergence — so (1) is testing the restore, not the driver.
func TestSliceMaterialisationNeedsItsBaseSnapshot(t *testing.T) {
	ctDir := boundarylog.BuildGrowMemRecording(t, t.TempDir(), growMemCalls)
	run := produceSlices(t, growMemWasm, ctDir, "grow_mem.wasm",
		wasmsnapshot.SlicePolicy{EveryPoints: 4})
	require.True(t, len(run.manifest.Slices) >= 3,
		"expected several slices, got %d", len(run.manifest.Slices))

	for i, s := range run.manifest.Slices {
		if s.BasePoint == 0 {
			// Point 0 IS a fresh instantiation, so restoring nothing is
			// correct there; the slice carries a base snapshot anyway (it must,
			// to be self-describing), but this test has nothing to prove about
			// it.
			continue
		}
		t.Run(fmt.Sprintf("slice-%d", i), func(t *testing.T) {
			alone := isolate(t, run, i)
			set, diag, err := wasmsnapshot.Load(alone, false)
			require.NoError(t, err)
			require.Equal(t, "", diag)
			base, end, hasBase := set.Range()
			require.True(t, hasBase)
			rec, ok := set.Nearest(base)
			require.True(t, ok)
			snap, err := set.Snapshot(rec)
			require.NoError(t, err)

			// (1) With the base snapshot restored: the range replays, every
			// state-dependent return value matches, and the module ends in the
			// state linear replay reaches.
			var seeded *wasmsnapshot.Snapshot
			h := newHarnessFor(t, growMemWasm, ctDir)
			res, err := boundarylog.Replay(h.ctx, boundarylog.Options{
				Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
				ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
				FromPoint:    base,
				ToPoint:      end,
				Resume:       func(mod api.Module) error { return snap.Restore(mod) },
				AtQuiescentPoint: func(p int, mod api.Module) error {
					if p != end {
						return nil
					}
					s, err := wasmsnapshot.Capture(mod, p)
					seeded = s
					return err
				},
			})
			require.NoError(t, err)
			require.Equal(t, end-base, res.ExportCalls)
			require.NotNil(t, seeded)

			linear := stateAtPoint(t, growMemWasm, ctDir, end)
			require.Equal(t, linear.MemoryBytes, seeded.MemoryBytes)
			if !bytes.Equal(linear.Memory, seeded.Memory) {
				t.Fatal("the state reached through the slice's base snapshot differs " +
					"from the state linear replay reaches at the slice's end")
			}
			require.Equal(t, linear.Globals, seeded.Globals)

			// (2) Without it: a divergence, at the slice's very first call.
			hb := newHarnessFor(t, growMemWasm, ctDir)
			_, err = boundarylog.Replay(hb.ctx, boundarylog.Options{
				Runtime: hb.rt, Compiled: hb.compiled, Recording: hb.rec,
				ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
				FromPoint:    base,
				ToPoint:      end,
				// A resume that restores nothing — the failure mode a pure
				// fixture cannot detect.
				Resume: func(api.Module) error { return nil },
			})
			require.Error(t, err)
			var div *boundarylog.DivergenceError
			require.True(t, errorsAs(err, &div),
				"replaying slice %d without its base snapshot produced %T: %v", i, err, err)
		})
	}
}

// stateAtPoint returns the module state linear replay reaches at `point`.
func stateAtPoint(t *testing.T, wasmPath, ctDir string, point int) *wasmsnapshot.Snapshot {
	t.Helper()
	var got *wasmsnapshot.Snapshot
	h := newHarnessFor(t, wasmPath, ctDir)
	_, err := boundarylog.Replay(h.ctx, boundarylog.Options{
		Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		AtQuiescentPoint: func(p int, mod api.Module) error {
			if p != point {
				return nil
			}
			s, err := wasmsnapshot.Capture(mod, p)
			got = s
			return err
		},
	})
	require.NoError(t, err)
	require.NotNil(t, got)
	return got
}

// TestSliceCarriesEveryPageItsSnapshotsReference pins the storage decision that
// independence actually rests on: each slice gets its **own** page-CAS
// per-trace tier.
//
// If slices shared one tier, slice N's regions would be `kind=2` references to
// pages that only slice 0's `wcppages.ns` carries, and slice N would be
// unreadable without it — a set of containers readable only in order, which is
// the opposite of what slicing is for. Loading with the system cache disabled
// makes the container's own store the only possible source, so this test fails
// if the tier is ever shared.
func TestSliceCarriesEveryPageItsSnapshotsReference(t *testing.T) {
	ctDir := boundarylog.BuildGrowMemRecording(t, t.TempDir(), growMemCalls)
	run := produceSlices(t, growMemWasm, ctDir, "grow_mem.wasm",
		wasmsnapshot.SlicePolicy{EveryPoints: 3})
	require.True(t, len(run.manifest.Slices) > 1)

	for i := range run.manifest.Slices {
		alone := isolate(t, run, i)
		set, diag, err := wasmsnapshot.Load(alone, false)
		require.NoError(t, err)
		require.Equal(t, "", diag)

		// Every snapshot-bearing point in the slice must materialise, not only
		// the base: a slice's interior snapshots are its cheaper entry points
		// and are just as useless if their pages live elsewhere.
		materialised := 0
		for _, r := range set.Points() {
			if !r.HasSnapshot() {
				continue
			}
			snap, err := set.Snapshot(r)
			require.NoError(t, err)
			require.Equal(t, r.MemoryBytes, uint64(len(snap.Memory)))
			materialised++
		}
		require.True(t, materialised > 0, "slice %d carries no snapshot at all", i)
	}
}

// TestSliceIsAValidStandaloneContainer mirrors `MCR-Client-Side-Splitting.md`
// M18c's first two checks: each slice is a valid CTFS container in its own
// right, readable as a plain file, with no archive or compression wrapping — so
// it is directly uploadable.
func TestSliceIsAValidStandaloneContainer(t *testing.T) {
	ctDir := boundarylog.BuildComputeBalanceRecording(t, t.TempDir(), callArgs)
	run := produceSlices(t, balanceCalcWasm, ctDir, "balance_calc.wasm",
		wasmsnapshot.SlicePolicy{EveryPoints: 2})

	for _, s := range run.manifest.Slices {
		path := filepath.Join(run.dir, s.File)
		raw, err := os.ReadFile(path)
		require.NoError(t, err)
		require.True(t, len(raw) > 0)
		// Not a zip (`PK\x03\x04`), not gzip (`\x1f\x8b`), not zstd
		// (`\x28\xb5\x2f\xfd`) — a bare container.
		require.False(t, bytes.HasPrefix(raw, []byte("PK\x03\x04")))
		require.False(t, bytes.HasPrefix(raw, []byte{0x1f, 0x8b}))
		require.False(t, bytes.HasPrefix(raw, []byte{0x28, 0xb5, 0x2f, 0xfd}))

		c, err := ctfs.Open(path)
		require.NoError(t, err)
		require.True(t, c.Has("meta.dat"), "slice %d has no meta.dat", s.Index)
		for _, ns := range wasmsnapshot.NamespaceNames() {
			require.True(t, c.Has(ns), "slice %d is missing %s", s.Index, ns)
		}
	}

	// The manifest names every slice file, and nothing else is in the
	// directory.
	entries := listTree(t, run.dir)
	require.Equal(t, len(run.manifest.Slices)+1, len(entries))
	reloaded, err := wasmsnapshot.LoadSliceManifest(
		filepath.Join(run.dir, wasmsnapshot.SliceManifestName))
	require.NoError(t, err)
	require.Equal(t, run.manifest.Slices, reloaded.Slices)
}
