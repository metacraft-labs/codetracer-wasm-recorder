// Seek and slice byte-identity over a module that has **both** DWARF and
// state — M38b's fourth deliverable.
//
// The M38 review found that the two existing fixtures cover different
// halves and neither covers both:
//
//   - `balance_calc.wasm` has DWARF, so its replay produces real
//     step/call/value streams and the byte-identity comparisons have
//     something to compare. But its only export is a **pure function of
//     its arguments**, so the trace it produces does not depend on the
//     state the range starts from. Measured: delete the memory copy from
//     `Snapshot.Restore` and `TestSnapshotMaterialisationIsByteIdentical`
//     still passes.
//   - `grow_mem.wasm` carries state across exported calls, so a slice
//     materialised without its base snapshot diverges. But it is a
//     hand-written `.wat` with no DWARF, so its `steps.dat`, `types.dat`
//     and `events.dat` are all zero bytes — the byte-identity
//     comparisons over it compare empty streams and pin only the
//     container scaffolding.
//
// So trace *content* was pinned on a stateless module and *state* on a
// contentless one. `tick_ledger` is both: twenty-four exported calls
// whose answers are `balance ^ checksum(tail)` — no argument determines
// one — compiled from Rust with `-C debuginfo=2`, so the same replay
// that must reproduce the state also emits ~2000 source-level steps.
//
// Every assertion here therefore fails for two independent reasons, and
// the second test is what proves it: with the snapshot restored the two
// containers are byte-identical; without it the replay diverges, and it
// diverges because the *values* differ, not because the shape does.
//
// The recording is the committed output of a real headless-Chromium
// session — `cmd/wazero/testdata/boundary-log/parity-corpus/`, produced
// by `codetracer/src/db-backend/tests/fixtures/wasm-parity-corpus/`. It
// is not built by `recordingBuilder`, unlike the `balance_calc` and
// `grow_mem` recordings these tests otherwise use, so these are also the
// first snapshot tests driven by a producer this repository did not
// write.
package boundarylog_test

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/boundarylog"
	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasmsnapshot"
	"github.com/tetratelabs/wazero/tracewriter"
)

const (
	corpusDir = "../../cmd/wazero/testdata/boundary-log/parity-corpus"
	// `tick_ledger` is the corpus member chosen here because it makes
	// twenty-four exported calls: enough quiescent points for several
	// snapshots and several slices, which is what a *seek* property
	// needs and what the three-to-six-call members cannot give.
	tickLedgerWasm      = corpusDir + "/tick_ledger/tick_ledger.wasm"
	tickLedgerRecording = corpusDir + "/tick_ledger/tick-ledger.ct"
	tickLedgerProgram   = "tick_ledger.wasm"
	// The recording's exported-call count, and so its last quiescent
	// point. Pinned rather than derived so a truncated recording is a
	// failure here rather than a quietly smaller test.
	tickLedgerCalls = 24
)

// TestSeekEquivalenceOverAStateCarryingModule is M38b's fourth
// deliverable and the milestone's `verify_seek_equivalence_over_a_state_carrying_module`.
//
// Both sides materialise the range [k, N). They differ in exactly one
// thing: how the module reaches quiescent point k — by re-executing
// calls 0..k, or by restoring the snapshot taken there. The produced
// containers are compared stream by stream, byte for byte.
func TestSeekEquivalenceOverAStateCarryingModule(t *testing.T) {
	work := t.TempDir()

	// A real container to attach the snapshot namespaces to: the trace
	// of the whole recording, produced the ordinary way.
	h := newHarnessFor(t, tickLedgerWasm, tickLedgerRecording)
	require.Equal(t, tickLedgerCalls, len(h.rec.TopLevelExports()),
		"the committed tick_ledger recording should carry %d exported calls",
		tickLedgerCalls)

	w := tracewriter.NewCtfsTraceWriter()
	_, err := boundarylog.Replay(h.ctx, boundarylog.Options{
		Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
		Recorder:     w,
	})
	require.NoError(t, err)
	base := produceAs(t, w, filepath.Join(work, "full"), tickLedgerProgram)

	n := deriveSnapshotsFor(t, tickLedgerWasm, tickLedgerRecording, base, false)
	require.Equal(t, tickLedgerCalls+1, n)

	set, diag, err := wasmsnapshot.Load(base, false)
	require.NoError(t, err)
	require.Equal(t, "", diag)
	require.NotNil(t, set)

	// A spread of seek targets rather than every one: early (the ring is
	// still filling), middle, and late (the ring has wrapped several
	// times and the balance has taken every branch of `tick`).
	for _, k := range []int{1, 5, 12, 23} {
		t.Run(fmt.Sprintf("from-point-%d", k), func(t *testing.T) {
			// --- linear side --------------------------------------------
			linearW := tracewriter.NewCtfsTraceWriter()
			hl := newHarnessFor(t, tickLedgerWasm, tickLedgerRecording)
			_, err := boundarylog.Replay(hl.ctx, boundarylog.Options{
				Runtime: hl.rt, Compiled: hl.compiled, Recording: hl.rec,
				ModuleConfig:     wazero.NewModuleConfig().WithStartFunctions(),
				AtQuiescentPoint: attachRecorderAt(t, k, linearW),
			})
			require.NoError(t, err)
			linear := produceAs(t, linearW,
				filepath.Join(work, fmt.Sprintf("linear-%d", k)), tickLedgerProgram)

			// --- snapshot-seeded side -----------------------------------
			rec, ok := set.Nearest(k)
			require.True(t, ok)
			require.Equal(t, uint32(k), rec.Ordinal)
			snap, err := set.Snapshot(rec)
			require.NoError(t, err)

			seededW := tracewriter.NewCtfsTraceWriter()
			hs := newHarnessFor(t, tickLedgerWasm, tickLedgerRecording)
			res, err := boundarylog.Replay(hs.ctx, boundarylog.Options{
				Runtime: hs.rt, Compiled: hs.compiled, Recording: hs.rec,
				ModuleConfig:     wazero.NewModuleConfig().WithStartFunctions(),
				FromPoint:        k,
				AtQuiescentPoint: attachRecorderAt(t, k, seededW),
				Resume:           func(mod api.Module) error { return snap.Restore(mod) },
			})
			require.NoError(t, err)
			require.Equal(t, k, res.FromPoint)
			require.Equal(t, tickLedgerCalls-k, res.ExportCalls)
			seeded := produceAs(t, seededW,
				filepath.Join(work, fmt.Sprintf("seeded-%d", k)), tickLedgerProgram)

			compareContainers(t, linear, seeded,
				"linear replay", "snapshot-seeded replay")

			// The comparison must not be comparing two empty documents.
			// `grow_mem`'s did, and that is the hole this test closes.
			requireNonEmptyTraceStreams(t, linear)
			requireNonEmptyTraceStreams(t, seeded)
		})
	}
}

// TestSeekOverAStateCarryingModuleWithoutItsSnapshotDiverges is the
// sensitivity floor under the test above.
//
// It is the half `balance_calc` could never show: a resume that restores
// nothing must make the replay fail, and fail as a **divergence** — the
// recorded return value of call k does not match what a module starting
// from scratch computes. Without this, byte-identity between two
// containers could be reporting that neither of them depended on the
// state at all.
func TestSeekOverAStateCarryingModuleWithoutItsSnapshotDiverges(t *testing.T) {
	for _, k := range []int{1, 5, 12, 23} {
		t.Run(fmt.Sprintf("from-point-%d", k), func(t *testing.T) {
			h := newHarnessFor(t, tickLedgerWasm, tickLedgerRecording)
			_, err := boundarylog.Replay(h.ctx, boundarylog.Options{
				Runtime: h.rt, Compiled: h.compiled, Recording: h.rec,
				ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
				FromPoint:    k,
				// A resume that restores nothing.
				Resume: func(api.Module) error { return nil },
			})
			require.Error(t, err,
				"seeking to point %d without restoring the snapshot must not succeed", k)
			var div *boundarylog.DivergenceError
			require.True(t, errorsAs(err, &div),
				"expected a divergence at point %d, got %T: %v", k, err, err)
		})
	}
}

// TestSliceTraceEqualsTheLinearTraceOfItsRangeOnAStateCarryingModule
// carries the same closure to slices.
//
// `TestSliceTraceEqualsTheLinearTraceOfItsRange` runs over
// `balance_calc`, so it compares real trace streams that do not depend
// on where the range starts; `TestSliceMaterialisationNeedsItsBaseSnapshot`
// runs over `grow_mem`, so it compares state but no trace. This runs the
// trace comparison over a module for which both halves are load-bearing
// at once.
func TestSliceTraceEqualsTheLinearTraceOfItsRangeOnAStateCarryingModule(t *testing.T) {
	run := produceSlices(t, tickLedgerWasm, tickLedgerRecording, tickLedgerProgram,
		wasmsnapshot.SlicePolicy{EveryPoints: 8})
	require.True(t, len(run.manifest.Slices) >= 3,
		"expected several slices, got %d", len(run.manifest.Slices))
	work := t.TempDir()

	for i, s := range run.manifest.Slices {
		t.Run(fmt.Sprintf("slice-%d", i), func(t *testing.T) {
			// Produced under the slice's own program name, because that
			// name reaches `meta.dat` and the slice carries the name it
			// was sealed with.
			want := materialiseLinearRange(t, tickLedgerWasm, tickLedgerRecording,
				s.BasePoint, s.EndPoint,
				filepath.Join(work, fmt.Sprintf("linear-%d", i)),
				wasmsnapshot.SliceProgramName(tickLedgerProgram, s.Index))
			got := filepath.Join(run.dir, s.File)
			compareContainers(t, want, got,
				"linear replay of the range", "the slice as produced")
			requireNonEmptyTraceStreams(t, want)
		})
	}
}

// requireNonEmptyTraceStreams asserts that a container carries real
// trace content, not only scaffolding.
//
// It exists because "the containers are byte-identical" is satisfied
// vacuously by two containers whose trace streams are both empty, which
// is precisely what every `grow_mem` comparison was doing.
func requireNonEmptyTraceStreams(t *testing.T, containerPath string) {
	t.Helper()
	c, err := ctfs.Open(containerPath)
	require.NoError(t, err)
	for _, name := range []string{"steps.dat", "types.dat"} {
		data, err := c.ReadFile(name)
		require.NoError(t, err, "%s carries no %s", containerPath, name)
		require.True(t, len(data) > 0,
			"%s: %s is empty — the byte-identity comparison over this container "+
				"would be comparing scaffolding, which is the `grow_mem` hole "+
				"M38b's fourth deliverable exists to close", containerPath, name)
	}
}
