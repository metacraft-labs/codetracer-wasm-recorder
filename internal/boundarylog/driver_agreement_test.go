package boundarylog

// The two replay drivers must accept and refuse the same things.
//
// `Replay` and `StreamingReplay` "differ in what they know, not in what they
// do" (`stream_replay.go`'s header). `TestStreamAgreesWithTheBatchParser` holds
// the *parsers* to that: the crossings recovered from a growing stream are the
// crossings `LoadRecording` recovers from the finished file. This file holds
// the *drivers* to it, which is a strictly stronger statement — two drivers can
// agree on every recovered crossing and still reach opposite verdicts, and for
// one shape of recording they did.
//
// M39 keyed the classification of an unmatched `() -> ()` import call on
// `Recording.MarkersIdentifyImports`, a witness derived from the whole
// recording. `LoadRecording` has the whole recording; a stream does not, so
// `StreamingReplay` was reading the witness off the groups that had arrived —
// and answering *permissively* (M37's "replayed unchecked and counted") on a
// question the batch driver answered strictly (a spec §6 divergence). M51.
//
// NO MOCKS. Both drivers run against the real committed `void_import.wasm`,
// through the same `recordingBuilder` that
// `TestBuilderReproducesTheCommittedBrowserRecording` pins to the real browser
// output. The only thing arranged is which call group carries the recording's
// first import crossing, which is the variable the defect was a function of.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/testing/require"
)

// verdict is a replay reduced to what the two drivers must agree on: whether
// it refused the recording, and what it did if it did not.
//
// The divergence *message* is deliberately not part of it. A streaming driver
// names the crossing it can see, and at the moment of a deferred call it has
// seen strictly fewer crossings than the batch driver has — so the two
// diagnostics can point at different crossings while agreeing perfectly on
// whether the recording is replayable. Requiring identical text would be
// requiring the streaming driver to know the future.
type verdict struct {
	failed               bool
	exportCalls          int
	importCalls          int
	uncheckedImportCalls int
}

// batchVerdict replays a finished recording through `Replay`.
func batchVerdict(t *testing.T, wasmPath, ctDir string) (verdict, error) {
	t.Helper()
	res, err := replayFixture(t, wasmPath, ctDir, nil)
	return verdict{
		failed:               err != nil,
		exportCalls:          res.ExportCalls,
		importCalls:          res.ImportCalls,
		uncheckedImportCalls: res.UncheckedImportCalls,
	}, err
}

// streamVerdict replays the same recording's bytes through `StreamingReplay`.
//
// The reader is a `bytes.Reader` over the exact file the batch driver read, so
// the only difference between the two runs is the driver: same module, same
// bytes, same order. `io.EOF` at the end of a `bytes.Reader` is precisely the
// "the producer has finished" contract `NewStreamReader` documents.
func streamVerdict(t *testing.T, wasmPath, ctDir string) (verdict, error) {
	t.Helper()
	ctx := context.Background()

	wasm, err := os.ReadFile(wasmPath)
	require.NoError(t, err)
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	t.Cleanup(func() { _ = rt.Close(ctx) })
	compiled, err := rt.CompileModule(ctx, wasm)
	require.NoError(t, err)

	rec, err := LoadRecordingMetadata(ctDir)
	require.NoError(t, err)

	raw, err := os.ReadFile(filepath.Join(ctDir, "trace.json"))
	require.NoError(t, err)

	res, err := StreamingReplay(ctx, Options{
		Runtime:      rt,
		Compiled:     compiled,
		Recording:    rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
	}, NewStreamReader(bytes.NewReader(raw)))
	require.Nil(t, res.Truncation, "the whole file was supplied; nothing is truncated")
	return verdict{
		failed:               err != nil,
		exportCalls:          res.ExportCalls,
		importCalls:          res.ImportCalls,
		uncheckedImportCalls: res.UncheckedImportCalls,
	}, err
}

// valuelessImportRecording builds `groups` successive `ping_n(1)` calls
// against `void_import.wasm`, recording the import crossing only from group
// `firstImport` onwards.
//
// The module calls `host_ping` — signature `() -> ()` — exactly once per call,
// always. So a group at or after `firstImport` is a *faithful* record of that
// call, and one before it is a recording that omits a crossing the module
// makes: the divergence M39 made the batch driver refuse.
//
// `firstImport == groups` therefore builds a recording with no import crossing
// at all, which is indistinguishable from a pre-M39 recording and is the case
// M39's backward-compatibility promise covers.
func valuelessImportRecording(t *testing.T, groups, firstImport int) string {
	t.Helper()
	b := newRecordingBuilder("valueless-import")
	for g := 0; g < groups; g++ {
		withCrossing := g >= firstImport
		b.export("ping_n", 0, "/src/void_import.wat", 1,
			[]jsValue{jsInt(1)}, []jsValue{jsInt(1)}, func() {
				if withCrossing {
					b.importCall(0, "/src/void_import.wat", 1, nil, nil)
				}
			})
	}
	return b.write(t, t.TempDir())
}

// TestBothDriversAgreeOnAValuelessImportDivergence is M51's property, stated
// over the variable the defect was a function of: **where in the stream the
// recording's first import crossing sits**.
//
// Before the fix, `firstImport` 1 and 2 failed here: the batch driver refused
// the recording with a `*DivergenceError` and the streaming driver returned
// `err == nil`, `UncheckedImportCalls == 1` and a replay it was willing to
// write a trace from. The verdicts were opposite, and the streaming one was the
// permissive of the two — a silent wrong answer on a path reachable from the
// CLI as `--boundary-stream`.
func TestBothDriversAgreeOnAValuelessImportDivergence(t *testing.T) {
	const groups = 3
	for _, tc := range []struct {
		name        string
		firstImport int
		wantFailed  bool
		// wantUnchecked applies only when the replay succeeds.
		wantUnchecked int
	}{
		{
			// Every group is a faithful record. Both drivers are strict from
			// their first call, because the witness is established by the very
			// first group.
			name: "first group carries it", firstImport: 0, wantFailed: false,
		},
		{
			// The reproduction the milestone was filed on: group 0 omits the
			// crossing its call makes, group 1 carries one. The batch driver
			// sees the marker before it drives anything; the streaming driver
			// cannot.
			name: "second group carries it", firstImport: 1, wantFailed: true,
		},
		{
			name: "third group carries it", firstImport: 2, wantFailed: true,
		},
		{
			// No import crossing anywhere: indistinguishable from a pre-M39
			// recording, so M39's compatibility promise applies and BOTH
			// drivers must accept it and count. This is the negative control —
			// it is what stops the fix from being "make the streaming driver
			// refuse everything".
			name: "no group carries it", firstImport: groups,
			wantFailed: false, wantUnchecked: groups,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctDir := valuelessImportRecording(t, groups, tc.firstImport)

			batch, batchErr := batchVerdict(t, voidImportWasm, ctDir)
			stream, streamErr := streamVerdict(t, voidImportWasm, ctDir)

			// The verdict itself — accepted or refused — is the property, and
			// it is checked unconditionally.
			require.Equal(t, batch.failed, stream.failed,
				"the two drivers disagree about the same recording and the same "+
					"module.\nbatch:     %+v (err: %v)\nstreaming: %+v (err: %v)",
				batch, batchErr, stream, streamErr)

			// The counts are compared only on an accepted replay, and
			// deliberately so. A refused replay writes no trace, so its tallies
			// describe abandoned work: the batch driver stops at the diverging
			// call and the streaming driver reports at end of stream, having
			// deferred rather than decided. Requiring those numbers to match
			// would be requiring the streaming driver to have stopped at a call
			// it could not yet classify — which is the bug, not the fix.
			if !batch.failed {
				require.Equal(t, batch, stream,
					"the two drivers accepted the same recording but did different "+
						"work.\nbatch:     %+v\nstreaming: %+v", batch, stream)
			}

			require.Equal(t, tc.wantFailed, batch.failed,
				"batch verdict: %+v (err: %v)", batch, batchErr)
			if tc.wantFailed {
				// Both refusals must be divergences, not incidental failures:
				// a recording refused for the wrong reason would satisfy the
				// equality above while meaning something else entirely.
				var d *DivergenceError
				require.True(t, errors.As(batchErr, &d),
					"batch: want a *DivergenceError, got %T: %v", batchErr, batchErr)
				require.True(t, errors.As(streamErr, &d),
					"streaming: want a *DivergenceError, got %T: %v", streamErr, streamErr)
				return
			}
			require.NoError(t, batchErr)
			require.NoError(t, streamErr)
			require.Equal(t, groups, batch.exportCalls)
			require.Equal(t, tc.wantUnchecked, batch.uncheckedImportCalls)
		})
	}
}

// TestATruncatedStreamCannotSettleTheImportFormat pins the one case
// `resolveDeferredValuelessImports` refuses rather than answers.
//
// A stream cut off before any import-labelled marker arrived leaves the
// witness false — but "false" there means "none had arrived yet", which is not
// the "none exists" that `LoadRecording` derives and that M37's unchecked-call
// fallback is licensed by. Promoting the one to the other would be a guess in
// the permissive direction, so the replay is refused and says why.
func TestATruncatedStreamCannotSettleTheImportFormat(t *testing.T) {
	ctDir := valuelessImportRecording(t, 3, 1)

	// Cut the stream after the first call group. That group's call to
	// `host_ping` is exactly the one whose classification has to be deferred,
	// and the marker that would settle it is in the part that never arrives.
	chunks := StreamChunksForRecording(t, ctDir)
	require.True(t, len(chunks) > 1, "need more than one group to cut between")

	ctx := context.Background()
	wasm, err := os.ReadFile(voidImportWasm)
	require.NoError(t, err)
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	t.Cleanup(func() { _ = rt.Close(ctx) })
	compiled, err := rt.CompileModule(ctx, wasm)
	require.NoError(t, err)
	rec, err := LoadRecordingMetadata(ctDir)
	require.NoError(t, err)

	_, err = StreamingReplay(ctx, Options{
		Runtime:      rt,
		Compiled:     compiled,
		Recording:    rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
	}, NewStreamReader(bytes.NewReader(chunks[0])))

	require.Error(t, err, "an unresolvable classification must not be accepted")
	require.True(t, strings.Contains(err.Error(), "could not be classified"),
		"the refusal must say what it could not decide; got: %v", err)
	var d *DivergenceError
	require.True(t, errors.As(err, &d),
		"the strict reading's finding must survive in the chain; got %T: %v", err, err)
}

// TestADanglingImportEnterMarkerIsRefused pins `closeDanglingImports`'s
// `markerBracketed && !force` exemption, which M51's review found unpinned:
// the whole `internal/boundarylog` suite stayed green with it deleted, even
// though deleting it changes behaviour.
//
// The exemption is what makes a marker-bracketed crossing closable by its
// `LEAVE` marker **and by nothing else**. Without it, an `ENTER` whose `LEAVE`
// never arrives is silently popped by the next value run that does not
// continue it, the enclosing export's `Return` then lands on a balanced stack,
// and the recording parses — with a crossing whose extent was invented. With
// it, the stack never rebalances and the recording is refused, which is spec
// §8's discipline: a recording whose crossing structure has to be guessed at is
// rejected, not repaired.
//
// The `force` half is exercised by end-of-input truncation
// (`StreamReader.finishStream`); this pins the `!force` half, which is the one
// no test reached.
func TestADanglingImportEnterMarkerIsRefused(t *testing.T) {
	b := newRecordingBuilder("dangling-enter")
	b.export("run", 1, "/src/void_import.wat", 1,
		[]jsValue{jsInt(7)}, []jsValue{jsInt(107)}, func() {
			// An `ENTER` marker for import #0 with no matching `LEAVE` — the
			// shape a producer killed inside a host call leaves behind.
			b.marker("recv", "wasm import #0", "")
			b.importCall(1, "/src/void_import.wat", 1,
				[]jsValue{jsInt(7), jsInt(100)}, []jsValue{jsInt(107)})
		})
	dir := b.write(t, t.TempDir())

	_, err := LoadRecording(dir)
	require.Error(t, err,
		"an import ENTER with no LEAVE must be refused, not silently repaired")
	require.True(t, strings.Contains(err.Error(), "no matching open export frame"),
		"got: %v", err)
}

// ── Review extensions ──────────────────────────────────────────────────────
//
// The table above varies one thing: which call GROUP carries the recording's
// first import crossing. Everything below varies the things it does not, and
// each of them is a way the deferral could have been lost or mis-resolved
// rather than a restatement of the same case.

// valuelessImportsWithinAGroup builds `groups` successive `ping_n(pings)`
// calls, recording only the first `recorded` of each group's `pings`
// crossings — except that group 0 records `recordedInFirstGroup`.
//
// It moves the divergence INSIDE a call group, which the position table
// cannot: there the deferred call is always a group's first, so the deferral
// is stashed before any crossing of that group has been consumed. Here the
// deferral is stashed with the cursor part-way through a group, which is the
// state the "the strict check must not advance the cursor" claim is about.
func valuelessImportsWithinAGroup(t *testing.T, groups, pings, recordedInFirstGroup int) string {
	t.Helper()
	b := newRecordingBuilder("valueless-within-group")
	for g := 0; g < groups; g++ {
		n := pings
		if g == 0 {
			n = recordedInFirstGroup
		}
		b.export("ping_n", 0, "/src/void_import.wat", 1,
			[]jsValue{jsInt(int32(pings))}, []jsValue{jsInt(int32(pings))}, func() {
				for i := 0; i < n; i++ {
					b.importCall(0, "/src/void_import.wat", 1, nil, nil)
				}
			})
	}
	return b.write(t, t.TempDir())
}

// TestBothDriversAgreeOnSeveralValuelessImportsInOneGroup drives the same
// property with MORE THAN ONE value-less import per call group, and with the
// first unmatched one at each position within the group.
//
// `recordedInFirstGroup == pings` is the faithful recording and both drivers
// must accept it; anything less omits a crossing the module makes, and both
// must refuse. The case that matters most is `pings-1`: the deferral is then
// stashed after the group has already had crossings consumed, so a strict
// check that advanced the cursor would leave the *following* calls matched
// against the wrong crossings — silently, since they are value-less.
func TestBothDriversAgreeOnSeveralValuelessImportsInOneGroup(t *testing.T) {
	const groups, pings = 3, 3
	for _, recorded := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprintf("first group records %d of %d", recorded, pings), func(t *testing.T) {
			ctDir := valuelessImportsWithinAGroup(t, groups, pings, recorded)

			batch, batchErr := batchVerdict(t, voidImportWasm, ctDir)
			stream, streamErr := streamVerdict(t, voidImportWasm, ctDir)

			require.Equal(t, batch.failed, stream.failed,
				"the two drivers disagree.\nbatch:     %+v (err: %v)\n"+
					"streaming: %+v (err: %v)", batch, batchErr, stream, streamErr)
			require.Equal(t, recorded != pings, batch.failed,
				"batch verdict: %+v (err: %v)", batch, batchErr)
			if !batch.failed {
				require.Equal(t, batch, stream,
					"accepted by both, but they did different work.\n"+
						"batch: %+v\nstreaming: %+v", batch, stream)
				require.Equal(t, groups*pings, batch.importCalls)
				require.Equal(t, 0, batch.uncheckedImportCalls)
			}
		})
	}
}

// mixedVintageRecording builds a recording of `run(x)` calls in which the
// value-carrying import crossing is spelled the PRE-M39 way (both realm
// markers say `wasm export #<n>`) in every group, and the value-less
// `host_ping` crossing is never recorded at all — plus, if `withM39Group`,
// one final group whose import crossing IS marker-labelled.
//
// `run` calls `host_ping` (import #0, `() -> ()`) and then `host_add`
// (import #1). So every group makes a value-less call the recording does not
// carry, and the crossing sitting at the cursor when it does is import #1 —
// a crossing the strict reading rejects on its INDEX rather than by running
// off the end of the recording, which is the other shape of stashed
// divergence and the one the position table never produces.
func mixedVintageRecording(t *testing.T, groups int, withM39Group bool) string {
	t.Helper()
	b := newRecordingBuilder("mixed-vintage")
	for g := 0; g < groups; g++ {
		b.export("run", 1, "/src/void_import.wat", 1,
			[]jsValue{jsInt(7)}, []jsValue{jsInt(107)}, func() {
				b.legacyImportCall(1, "/src/void_import.wat", 1,
					[]jsValue{jsInt(7), jsInt(100)}, []jsValue{jsInt(107)})
			})
	}
	if withM39Group {
		b.export("run", 1, "/src/void_import.wat", 1,
			[]jsValue{jsInt(7)}, []jsValue{jsInt(107)}, func() {
				b.importCall(0, "/src/void_import.wat", 1, nil, nil)
				b.importCall(1, "/src/void_import.wat", 1,
					[]jsValue{jsInt(7), jsInt(100)}, []jsValue{jsInt(107)})
			})
	}
	return b.write(t, t.TempDir())
}

// TestBothDriversAgreeOnAMixedRecording covers a recording that interleaves a
// value-less crossing with a value-carrying one, in both vintages.
//
// Two halves, and the second is the one the review added:
//
//   - **pure pre-M39** — no import-labelled marker anywhere, so the witness
//     ends false on a complete stream. Both drivers must ACCEPT, count one
//     unchecked call per group, and consume the value-carrying crossings
//     normally. This is also the end-to-end proof that the speculative strict
//     check leaves no trace: it fails here (import index 1 where the call is
//     to import #0) at a cursor sitting on a crossing the *next* call needs.
//     A check that advanced the cursor would eat `host_add`'s crossing and the
//     replay would diverge; it does not, so the replay is byte-for-byte the
//     work the batch driver did.
//   - **mixed** — the same groups plus a trailing M39 group. The witness ends
//     true, so both drivers must refuse. The batch driver refuses at the first
//     group's `host_ping`; the streaming driver's replay reaches the trailing
//     group and diverges there, with the classification still pending. That is
//     the "a divergence is raised later while a deferral is outstanding" exit
//     path, and the verdicts still agree.
func TestBothDriversAgreeOnAMixedRecording(t *testing.T) {
	const groups = 2
	t.Run("pre-M39 throughout: accepted, unchecked, cursor intact", func(t *testing.T) {
		ctDir := mixedVintageRecording(t, groups, false)

		batch, batchErr := batchVerdict(t, voidImportWasm, ctDir)
		stream, streamErr := streamVerdict(t, voidImportWasm, ctDir)

		require.NoError(t, batchErr)
		require.NoError(t, streamErr)
		require.Equal(t, batch, stream,
			"batch: %+v\nstreaming: %+v", batch, stream)
		// One `host_add` crossing consumed per group and one `host_ping` call
		// counted per group. The first is what a cursor-advancing speculative
		// check would have destroyed.
		require.Equal(t, groups, batch.importCalls)
		require.Equal(t, groups, batch.uncheckedImportCalls)
		require.Equal(t, groups, batch.exportCalls)
	})

	t.Run("mixed vintages: refused by both", func(t *testing.T) {
		ctDir := mixedVintageRecording(t, groups, true)

		batch, batchErr := batchVerdict(t, voidImportWasm, ctDir)
		stream, streamErr := streamVerdict(t, voidImportWasm, ctDir)

		require.Equal(t, batch.failed, stream.failed,
			"the two drivers disagree.\nbatch:     %+v (err: %v)\n"+
				"streaming: %+v (err: %v)", batch, batchErr, stream, streamErr)
		require.True(t, batch.failed, "batch accepted a mixed recording: %+v", batch)
		var d *DivergenceError
		require.True(t, errors.As(batchErr, &d),
			"batch: want a *DivergenceError, got %T: %v", batchErr, batchErr)
		require.True(t, errors.As(streamErr, &d),
			"streaming: want a *DivergenceError, got %T: %v", streamErr, streamErr)
	})
}

// TestTheStreamingWitnessEqualsTheBatchWitness tests the claim the whole
// resolution rests on: that the witness `StreamingReplay` has accumulated when
// the stream ends is the one `LoadRecording` derives from the same bytes.
//
// It is asserted directly rather than inferred from the verdicts, because it
// is the *reason* the verdicts agree — the two derivations are different code
// (`assembler.sawImportMarker` against a scan of `Crossing.markerBracketed` in
// `appendGroup`), and nothing but this makes them the same statement.
//
// The recordings deliberately include ones both drivers refuse: the witness
// has to be right there too, since it is what decides the refusal.
func TestTheStreamingWitnessEqualsTheBatchWitness(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ctDir func(t *testing.T) string
		want  bool
	}{
		{"marked from the first group", func(t *testing.T) string {
			return valuelessImportRecording(t, 3, 0)
		}, true},
		{"marked from the second group", func(t *testing.T) string {
			return valuelessImportRecording(t, 3, 1)
		}, true},
		{"marked only in the last group", func(t *testing.T) string {
			return valuelessImportRecording(t, 3, 2)
		}, true},
		{"never marked", func(t *testing.T) string {
			return valuelessImportRecording(t, 3, 3)
		}, false},
		{"pre-M39 spelling throughout", func(t *testing.T) string {
			return mixedVintageRecording(t, 2, false)
		}, false},
		{"pre-M39 groups then an M39 one", func(t *testing.T) string {
			return mixedVintageRecording(t, 2, true)
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctDir := tc.ctDir(t)

			loaded, err := LoadRecording(ctDir)
			require.NoError(t, err)
			require.Equal(t, tc.want, loaded.MarkersIdentifyImports,
				"LoadRecording's witness")

			// `streamVerdict` runs `StreamingReplay`, which accumulates the
			// witness into the metadata-only recording it is given. The error
			// is deliberately ignored: a refused replay still consumed the
			// whole stream, and the witness is what the refusal was decided
			// on.
			ctx := context.Background()
			wasm, err := os.ReadFile(voidImportWasm)
			require.NoError(t, err)
			rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
			t.Cleanup(func() { _ = rt.Close(ctx) })
			compiled, err := rt.CompileModule(ctx, wasm)
			require.NoError(t, err)
			rec, err := LoadRecordingMetadata(ctDir)
			require.NoError(t, err)
			raw, err := os.ReadFile(filepath.Join(ctDir, "trace.json"))
			require.NoError(t, err)
			_, _ = StreamingReplay(ctx, Options{
				Runtime:      rt,
				Compiled:     compiled,
				Recording:    rec,
				ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
			}, NewStreamReader(bytes.NewReader(raw)))

			require.Equal(t, loaded.MarkersIdentifyImports, rec.MarkersIdentifyImports,
				"the streaming witness at end of stream differs from "+
					"LoadRecording's, so the resolution is deciding on a "+
					"different question than the batch driver answered")
		})
	}
}

// TestTheSpeculativeStrictCheckLeavesNoTrace pins `matchImportCrossing`'s
// side-effect freedom directly, at the level the claim is made at.
//
// The end-to-end version of this is in `TestBothDriversAgreeOnAMixedRecording`
// — a spurious cursor advance there eats a crossing a later call needs. This
// is the unit statement: the check may read, and may not write.
func TestTheSpeculativeStrictCheckLeavesNoTrace(t *testing.T) {
	ctDir := valuelessImportRecording(t, 2, 0)
	rec, err := LoadRecording(ctDir)
	require.NoError(t, err)
	require.True(t, len(rec.Crossings) > 1)

	r := &replayer{opts: Options{Recording: rec}, providers: map[string]api.Module{}}
	// Sit the cursor on the recording's first import crossing and ask about a
	// call to a DIFFERENT import, so the check takes its failing path — the
	// one the deferral actually runs.
	r.cursor = 1
	require.Equal(t, CrossingImport, rec.Crossings[1].Kind)

	before := *r
	c, err := r.matchImportCrossing(&importPlan{
		index: rec.Crossings[1].Index + 1,
		sig:   Signature{},
	}, nil)
	require.Error(t, err, "the speculative check must have failed for this to mean anything")
	require.Nil(t, c)
	require.Equal(t, before.cursor, r.cursor, "the check advanced the cursor")
	require.Equal(t, before.result, r.result, "the check altered the tallies")
	require.Equal(t, before.err, r.err, "the check raised the divergence instead of returning it")

	// And the accepting path is equally inert: it reports the crossing without
	// consuming it. `serviceImport` is what advances the cursor, afterwards.
	c, err = r.matchImportCrossing(&importPlan{
		index: rec.Crossings[1].Index,
		sig:   Signature{},
	}, nil)
	require.NoError(t, err)
	require.Equal(t, &rec.Crossings[1], c)
	require.Equal(t, before.cursor, r.cursor, "the accepting check advanced the cursor")
	require.Equal(t, before.result, r.result, "the accepting check altered the tallies")
}

// TestATruncatedStreamWithASettledWitnessStillDiverges is the other half of
// `TestATruncatedStreamCannotSettleTheImportFormat`.
//
// Truncation does not itself make a classification unresolvable — it does so
// only when the witness is still false. Cut the stream AFTER the group that
// carries the marker and the witness is settled, so the stashed divergence is
// the answer and the truncation is beside the point. Without this the
// truncation arm could be over-broad (refusing every truncated stream with a
// pending deferral) and no test would notice.
func TestATruncatedStreamWithASettledWitnessStillDiverges(t *testing.T) {
	ctDir := valuelessImportRecording(t, 3, 1)
	chunks := StreamChunksForRecording(t, ctDir)
	require.True(t, len(chunks) > 2, "need at least two groups plus a tail")

	// Groups 0 and 1, with no closing bracket: group 0 defers, group 1 carries
	// the import-labelled marker that settles the witness, and the document
	// never ends.
	var prefix []byte
	prefix = append(prefix, chunks[0]...)
	prefix = append(prefix, chunks[1]...)

	ctx := context.Background()
	wasm, err := os.ReadFile(voidImportWasm)
	require.NoError(t, err)
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	t.Cleanup(func() { _ = rt.Close(ctx) })
	compiled, err := rt.CompileModule(ctx, wasm)
	require.NoError(t, err)
	rec, err := LoadRecordingMetadata(ctDir)
	require.NoError(t, err)

	out, err := StreamingReplay(ctx, Options{
		Runtime:      rt,
		Compiled:     compiled,
		Recording:    rec,
		ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
	}, NewStreamReader(bytes.NewReader(prefix)))

	require.NotNil(t, out.Truncation, "the prefix was cut short")
	require.True(t, rec.MarkersIdentifyImports, "group 1 settles the witness")
	require.Error(t, err)
	var d *DivergenceError
	require.True(t, errors.As(err, &d),
		"a settled witness must yield the stashed divergence, not the "+
			"unresolvable-classification refusal; got %T: %v", err, err)
	require.False(t, strings.Contains(err.Error(), "could not be classified"),
		"the classification WAS settled; got: %v", err)
}

// TestNoAcceptedStreamingReplayLeavesAClassificationPending is the invariant
// the whole milestone reduces to: **a streaming replay that returns nil never
// had a deferred classification it did not resolve.** A nil return with one
// outstanding is the original defect exactly — a permissive answer to a
// question the batch driver answered strictly — so it is asserted directly
// rather than inferred from a pair of verdicts.
//
// A deferral is outstanding for exactly the replays that made an unmatched
// `() -> ()` import call, and those are exactly the replays that counted one:
// `serviceUnwitnessedValuelessImport` stashes on the first and increments on
// every one. So `UncheckedImportCalls > 0` is the observable form of "a
// classification was deferred", and the contract is that an ACCEPTED replay
// reporting one must have resolved it the only way it may be resolved
// permissively — a false witness on a stream that actually ended.
//
// Every proper prefix of every recording in this file is driven, so each of
// the three truncation classes the stream classifier distinguishes is reached
// with a deferral pending.
func TestNoAcceptedStreamingReplayLeavesAClassificationPending(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ctDir func(t *testing.T) string
	}{
		{"marked from the first group", func(t *testing.T) string {
			return valuelessImportRecording(t, 3, 0)
		}},
		{"marked from the second group", func(t *testing.T) string {
			return valuelessImportRecording(t, 3, 1)
		}},
		{"marked only in the last group", func(t *testing.T) string {
			return valuelessImportRecording(t, 3, 2)
		}},
		{"never marked", func(t *testing.T) string {
			return valuelessImportRecording(t, 3, 3)
		}},
		{"several per group, one missing", func(t *testing.T) string {
			return valuelessImportsWithinAGroup(t, 3, 3, 2)
		}},
		{"pre-M39 spelling throughout", func(t *testing.T) string {
			return mixedVintageRecording(t, 2, false)
		}},
		{"pre-M39 groups then an M39 one", func(t *testing.T) string {
			return mixedVintageRecording(t, 2, true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctDir := tc.ctDir(t)
			ctx := context.Background()
			wasmBytes, err := os.ReadFile(voidImportWasm)
			require.NoError(t, err)
			rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
			t.Cleanup(func() { _ = rt.Close(ctx) })
			compiled, err := rt.CompileModule(ctx, wasmBytes)
			require.NoError(t, err)
			raw, err := os.ReadFile(filepath.Join(ctDir, "trace.json"))
			require.NoError(t, err)

			// The whole document, and then every proper prefix of it by call
			// group — a stream cut between two groups, which is where a
			// pending deferral meets a truncation.
			chunks := StreamChunksForRecording(t, ctDir)
			prefixes := [][]byte{raw}
			for n := 1; n < len(chunks); n++ {
				var prefix []byte
				for _, c := range chunks[:n] {
					prefix = append(prefix, c...)
				}
				prefixes = append(prefixes, prefix)
			}
			// And one cut INSIDE a record, so `TruncatedMidRecord` is reached
			// too rather than only the between-groups classes.
			if len(chunks) > 1 {
				var head []byte
				head = append(head, chunks[0]...)
				head = append(head, chunks[1][:len(chunks[1])/2]...)
				prefixes = append(prefixes, head)
			}

			for i, prefix := range prefixes {
				rec := mustReloadMetadata(t, ctDir)
				out, err := StreamingReplay(ctx, Options{
					Runtime: rt, Compiled: compiled,
					Recording:    rec,
					ModuleConfig: wazero.NewModuleConfig().WithStartFunctions(),
				}, NewStreamReader(bytes.NewReader(prefix)))
				if err != nil {
					continue
				}
				if out.UncheckedImportCalls == 0 {
					continue
				}
				// Accepted, with a classification that had to be deferred.
				// The only licensed resolution is M37\'s: a witness that
				// really is false, on a stream that really did end.
				require.False(t, rec.MarkersIdentifyImports,
					"prefix %d: accepted %d unchecked value-less import call(s) "+
						"in a recording whose markers DO name the import edge — "+
						"the batch driver refuses that, so the deferral was "+
						"mis-resolved", i, out.UncheckedImportCalls)
				require.Nil(t, out.Truncation,
					"prefix %d: accepted %d unchecked value-less import call(s) "+
						"off a TRUNCATED stream — a prefix cannot settle the "+
						"witness, so the deferral was dropped", i,
					out.UncheckedImportCalls)
			}
		})
	}
}

// mustReloadMetadata gives each probe its own metadata-only recording, since
// `StreamingReplay` appends the crossings it consumes into the one it is
// given.
func mustReloadMetadata(t *testing.T, ctDir string) *Recording {
	t.Helper()
	rec, err := LoadRecordingMetadata(ctDir)
	require.NoError(t, err)
	return rec
}
