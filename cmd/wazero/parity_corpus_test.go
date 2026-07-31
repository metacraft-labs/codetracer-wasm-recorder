// parity_corpus_test.go — the M45 verification suite: spec §10
// cross-modality parity over a corpus rather than over one module.
//
// Spec §10 states parity as a general property:
//
//	Record module M in the browser, producing a boundary log. Re-execute M
//	from that log in codetracer-wasm-recorder, producing trace A. Record M
//	directly in codetracer-wasm-recorder with a live host, producing trace
//	B. A and B must be equal modulo timestamps.
//
// Until M45 it was demonstrated on exactly one module, `balance_calc`,
// whose only export is a pure function of its two scalar arguments. A
// pure module is the weakest possible witness for a replay: its trace
// does not depend on the state the call starts from, so the comparison
// cannot distinguish a working entry state from none. The complementary
// hole was on the other side — `internal/wasmsnapshot/testdata/grow_mem.wasm`
// carries state but has no DWARF, so its `steps.dat`, `types.dat` and
// `events.dat` are zero bytes and a comparison over it pins only the
// container scaffolding. Trace *content* was pinned on a stateless
// module and *state* on a contentless one, and nothing on both.
//
// The four modules here have both. Each carries state across exported
// calls and each has DWARF describing interior structure the boundary
// recording never mentions:
//
//	loop_digest   loops and a three-deep call nest
//	pair_stats    a MULTI-VALUE export — the boundary carries a result tuple
//	vault_apply   an imported memory the host stages (§3.3) and an import
//	              that answers by writing into it (§3.4)
//	tick_ledger   twenty-four exported calls, so a replay spans several
//	              snapshots and several slices
//
// NO MOCKS. Every recording under `testdata/boundary-log/parity-corpus/`
// is the committed output of a real headless-Chromium session driven
// through the real `record-web` daemon; see that directory's README for
// the pipeline. Trace B is produced by driving wazero's ordinary public
// API — deliberately NOT through `internal/boundarylog`, since comparing
// a replay against itself would prove nothing.
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/testing/binaryencoding"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/tracewriter"
)

const corpusTestdata = boundaryTestdata + "/parity-corpus"

// liveHost is the "record M directly with a live host" half of §10 for a
// module that has imports. `nil` means the module imports nothing, which
// is the case for three of the four.
//
// It is deliberately an interface over the *public* wazero API: a live
// host that reached into `internal/boundarylog` would share the code
// under test between the two legs of the comparison, and the comparison
// would then be able to agree while both legs were wrong.
type liveHost interface {
	// install builds the modules the guest imports. Called after the
	// runtime exists and before the guest is instantiated.
	install(t *testing.T, ctx context.Context, rt wazero.Runtime)
	// stage runs after the guest is instantiated and before the first
	// exported call — the window spec §3.3 defines.
	stage(t *testing.T, guest api.Module)
}

// corpusModule describes one member of the parity corpus.
type corpusModule struct {
	// name is the directory under `testdata/boundary-log/parity-corpus/`
	// and the base name of the committed `.wasm`.
	name string
	// recording is the `.ct` directory the browser session produced. It
	// is named after the page's `program`, which uses hyphens where the
	// module uses underscores.
	recording string
	// export is the module's single exported function.
	export string
	// calls are the arguments the browser used, in order. They are
	// repeated here rather than recovered from the recording because
	// trace B must be driven by something the recording did not supply;
	// recovering them from it would make the direct leg a second reader
	// of the boundary log.
	calls [][]uint64
	// guard is a three-call sequence `[a, b, a]` whose first and third
	// results must differ. See TestVerifyTheCorpusModulesCarryDwarfAndState.
	guard [][]uint64
	// host supplies the module's imports for the direct leg.
	host func() liveHost
}

// parityCorpus is the corpus itself.
//
// Adding a module here is all that is needed to bring it under both
// checks below. Removing the last state-carrying or last DWARF-bearing
// one is what the guard test exists to catch.
var parityCorpus = []corpusModule{
	{
		name:      "loop_digest",
		recording: "loop-digest.ct",
		export:    "absorb",
		// `page/app.js`'s SAMPLES.
		calls: [][]uint64{{3}, {11}, {29}, {7}, {3}, {101}},
		guard: [][]uint64{{3}, {11}, {3}},
	},
	{
		name:      "pair_stats",
		recording: "pair-stats.ct",
		export:    "sample_pair",
		calls:     [][]uint64{{40}, {12}, {91}, {40}, {7}},
		guard:     [][]uint64{{40}, {12}, {40}},
	},
	{
		name:      "vault_apply",
		recording: "vault-apply.ct",
		export:    "apply_slot",
		calls:     [][]uint64{{0}, {1}, {2}},
		// Applying slot 0 twice: the second answer includes the first in
		// the running total, so it can only match the first if the
		// module's state were not carried.
		guard: [][]uint64{{0}, {1}, {0}},
		host:  func() liveHost { return &vaultLiveHost{} },
	},
	{
		name:      "tick_ledger",
		recording: "tick-ledger.ct",
		export:    "tick",
		calls: [][]uint64{
			{17}, {4}, {250}, {33}, {8}, {91}, {17}, {512}, {6}, {44}, {120}, {3},
			{77}, {17}, {900}, {21}, {5}, {64}, {1000}, {12}, {38}, {7}, {256}, {19},
		},
		guard: [][]uint64{{17}, {4}, {17}},
	},
}

func (m corpusModule) wasmPath() string {
	return filepath.Join(corpusTestdata, m.name, m.name+".wasm")
}

func (m corpusModule) recordingPath() string {
	return filepath.Join(corpusTestdata, m.name, m.recording)
}

// expectedAnswers reads what the browser observed.
//
// Every entry is a list even for a single-result export, so
// `pair_stats`' two results need no special case here — see the
// normalisation note in the fixture's `page-shared/harness.js`.
func (m corpusModule) expectedAnswers(t *testing.T) [][]uint64 {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(corpusTestdata, m.name, "expected.json"))
	require.NoError(t, err)
	var answers [][]uint64
	require.NoError(t, json.Unmarshal(raw, &answers))
	require.True(t, len(answers) == len(m.calls),
		"%s: expected.json has %d answers but the corpus declares %d calls — "+
			"the recording and this table have drifted apart",
		m.name, len(answers), len(m.calls))
	return answers
}

// ===========================================================================
// The direct leg: record M by running it under wazero with a live host
// ===========================================================================

// recordDirectCalls drives wazero's ordinary public API — compile,
// instantiate with the recorder attached, call, write the bundle — for a
// SEQUENCE of calls against one instance.
//
// The sequence is what `recordDirectInvocation` in boundary_log_test.go
// cannot express, and it is the whole point of this corpus: a module
// that carries state across calls has no interesting single-call
// behaviour, and one call could not witness that the replay reproduced
// what earlier calls left behind.
func recordDirectCalls(
	t *testing.T, m corpusModule, outDir string,
) [][]uint64 {
	return recordDirectCallsAt(t, m, m.wasmPath(), outDir)
}

func recordDirectCallsAt(
	t *testing.T, m corpusModule, wasmPath, outDir string,
) [][]uint64 {
	t.Helper()
	ctx := context.Background()

	wasmBytes, err := os.ReadFile(wasmPath)
	require.NoError(t, err)

	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer func() { require.NoError(t, rt.Close(ctx)) }()

	var host liveHost
	if m.host != nil {
		host = m.host()
		host.install(t, ctx, rt)
	}

	compiled, err := rt.CompileModule(ctx, wasmBytes)
	require.NoError(t, err)

	recorder := tracewriter.NewCtfsTraceWriter()
	guest, err := rt.InstantiateModuleWithRecord(ctx, compiled,
		wazero.NewModuleConfig().WithStartFunctions(), recorder)
	require.NoError(t, err)

	if host != nil {
		host.stage(t, guest)
	}

	fn := guest.ExportedFunction(m.export)
	require.True(t, fn != nil, "%s exports no %q", m.name, m.export)

	results := make([][]uint64, 0, len(m.calls))
	for i, args := range m.calls {
		res, err := fn.Call(ctx, args...)
		require.NoError(t, err, "%s: call %d (%v) failed", m.name, i, args)
		results = append(results, res)
	}

	workdir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, recorder.ProduceTrace(outDir, m.name+".wasm", workdir))
	return results
}

// ---------------------------------------------------------------------------
// `vault_apply`'s live host
// ---------------------------------------------------------------------------

// Layout of `Slot` in the module's `lib.rs`. Duplicated here rather than
// derived, because the whole point of the module is that the host and
// the guest agree on a layout neither computes from the other.
const (
	vaultSlotStride  = 16
	vaultOffKey      = 0
	vaultOffAmount   = 4
	vaultOffRateBps  = 8
	vaultRateOK      = 1
	vaultMemoryPages = 2
	vaultMaxPages    = 4
)

// vaultRequests is what `page/app.js` staged, and vaultRates is the host
// rate table it answered from. Neither is in the `.wasm`.
var (
	vaultRequests = []struct{ key, amount uint32 }{
		{7001, 480_000},
		{7002, 15_500},
		{7003, 96_240},
	}
	vaultRates = map[uint32]uint32{7001: 250, 7002: 40, 7003: 1_125}
)

// vaultLiveHost is the Go equivalent of `vault_apply`'s browser page: it
// owns the linear memory, stages the calldata in it before the first
// call, and answers `fetch_rate` by writing into it.
//
// It supplies the memory and the function from two different modules,
// which is what `lib.rs` is built for. wazero's `HostModuleBuilder` can
// export functions and nothing else, so a module that imported both from
// one namespace would need a synthesised provider — and the only one in
// the tree is `internal/boundarylog/provider.go`, whose behaviour is
// under test on the other leg of this comparison.
type vaultLiveHost struct {
	mem       api.Memory
	vaultBase uint32
}

func (h *vaultLiveHost) install(t *testing.T, ctx context.Context, rt wazero.Runtime) {
	t.Helper()

	// `env`: a module whose only content is the memory the guest
	// imports. `rust-lld --import-memory` always names it `env.memory`.
	envBytes := binaryencoding.EncodeModule(&wasm.Module{
		MemorySection: &wasm.Memory{
			Min: vaultMemoryPages, Cap: vaultMemoryPages,
			Max: vaultMaxPages, IsMaxEncoded: true,
		},
		ExportSection: []wasm.Export{
			{Type: wasm.ExternTypeMemory, Name: "memory", Index: 0},
		},
	})
	envMod, err := rt.InstantiateWithConfig(ctx, envBytes,
		wazero.NewModuleConfig().WithName("env"))
	require.NoError(t, err)
	h.mem = envMod.ExportedMemory("memory")
	require.True(t, h.mem != nil, "the env module must export its memory")

	// `host`: the rate lookup. It returns only a status code and
	// delivers its real answer by writing into the memory above, which
	// is what makes the recording need spec §3.4.
	_, err = rt.NewHostModuleBuilder("host").
		NewFunctionBuilder().
		WithFunc(func(key uint32) uint32 {
			rate, ok := vaultRates[key]
			if !ok {
				return 0
			}
			// The host finds the slot it staged itself; the guest never
			// told it an index.
			for i := range vaultRequests {
				base := h.vaultBase + uint32(i*vaultSlotStride)
				staged, ok := h.mem.ReadUint32Le(base + vaultOffKey)
				if !ok || staged != key {
					continue
				}
				if !h.mem.WriteUint32Le(base+vaultOffRateBps, rate) {
					return 0
				}
				return vaultRateOK
			}
			return 0
		}).
		Export("fetch_rate").
		Instantiate(ctx)
	require.NoError(t, err)
}

func (h *vaultLiveHost) stage(t *testing.T, guest api.Module) {
	t.Helper()
	// `rust-lld` exports the address of the `VAULT` symbol as a global.
	// Reading it crosses no boundary, which is what lets the host stage
	// its calldata before the first exported call.
	g := guest.ExportedGlobal("VAULT")
	require.True(t, g != nil, "vault_apply must export its VAULT global")
	h.vaultBase = uint32(g.Get())

	for i, req := range vaultRequests {
		base := h.vaultBase + uint32(i*vaultSlotStride)
		require.True(t, h.mem.WriteUint32Le(base+vaultOffKey, req.key))
		require.True(t, h.mem.WriteUint32Le(base+vaultOffAmount, req.amount))
	}
}

// ===========================================================================
// verify_parity_over_the_whole_corpus
// ===========================================================================

// TestVerifyParityOverTheWholeCorpus is the M45 headline property.
//
// For every corpus module: the trace materialised from its browser
// boundary recording must equal the trace recorded by running the same
// module directly under wazero with a live host, over the ENTIRE decoded
// document — path table, function table, variable-name table, every
// count, the meta.dat flags, and every event with all its fields.
//
// "Modulo timestamps" costs nothing to honour because `ct-print --full`
// surfaces no timestamps at all; the only exclusions are the bundle's
// own identity (program name, workdir), which name where the trace was
// written rather than what was executed.
func TestVerifyParityOverTheWholeCorpus(t *testing.T) {
	requireCtPrint(t)
	for _, m := range parityCorpus {
		t.Run(m.name, func(t *testing.T) {
			tmp := t.TempDir()

			// --- trace A: materialised from the browser recording -----
			outA := filepath.Join(tmp, "from-boundary-log")
			exitCode, _, stderr := runMain(t, "", []string{
				"run", "--boundary-log=" + m.recordingPath(),
				"--out-dir=" + outA, m.wasmPath()})
			require.Equal(t, 0, exitCode,
				"boundary-log replay of %s failed; stderr:\n%s", m.name, stderr)
			docA := dumpFull(t, outA)

			// --- trace B: recorded by running the module directly -----
			outB := filepath.Join(tmp, "direct")
			got := recordDirectCalls(t, m, outB)
			require.Equal(t, m.expectedAnswers(t), got,
				"%s: the direct run must compute what the browser observed", m.name)
			docB := dumpFull(t, outB)

			// ----- the two documents must agree in every particular ---
			require.Equal(t, docB.Paths, docA.Paths, "%s: path tables differ", m.name)
			require.Equal(t, docB.Functions, docA.Functions, "%s: function tables differ", m.name)
			require.Equal(t, docB.Varnames, docA.Varnames, "%s: variable-name tables differ", m.name)
			require.Equal(t, docB.Counts, docA.Counts, "%s: counts differ", m.name)
			require.Equal(t, docB.Metadata.Flags, docA.Metadata.Flags,
				"%s: meta.dat flags differ", m.name)

			require.Equal(t, len(docB.Events), len(docA.Events),
				"%s: event counts differ", m.name)
			for i := range docB.Events {
				require.Equal(t, string(docB.Events[i]), string(docA.Events[i]),
					"%s: event %d differs between the boundary-log replay and the "+
						"direct run.\n  direct:       %s\n  boundary-log: %s",
					m.name, i, string(docB.Events[i]), string(docA.Events[i]))
			}

			// A parity test that compared two empty documents would pass
			// vacuously. `TestVerifyTheCorpusModulesCarryDwarfAndState`
			// is the real guard; this is the cheap local one.
			require.True(t, docA.Counts["steps"] > 0, "%s: trace A has no steps", m.name)
			require.True(t, docA.Counts["calls"] >= len(m.calls),
				"%s: trace A should carry at least one frame per exported call", m.name)
		})
	}
}

// ===========================================================================
// verify_the_corpus_modules_carry_dwarf_and_state
// ===========================================================================

// corpusTraceStreams are the container's internal files that carry trace
// *content* as opposed to scaffolding.
//
// They are named literally because the M38 review's finding was literal:
// `grow_mem.wasm`'s `steps.dat`, `types.dat` and `events.dat` measured
// zero bytes, and every byte-identity comparison over it was therefore
// comparing two empty streams while looking exactly like a comparison of
// two traces.
var corpusTraceStreams = []string{"steps.dat", "types.dat"}

// TestVerifyTheCorpusModulesCarryDwarfAndState is the guard that keeps
// the corpus from silently degrading into another `balance_calc` (state-
// less) or another `grow_mem` (DWARF-less).
//
// It asserts two independent properties per module, both of which the
// M45 milestone names:
//
//  1. **DWARF.** The materialised container's `steps.dat` and
//     `types.dat` are non-empty, measured as byte counts inside the
//     `.ct` rather than inferred from a decoded count — a container can
//     report counts from an index while its streams are empty, and the
//     defect this guards against was exactly a zero-byte stream.
//
//  2. **State.** Calling the export with the SAME argument twice, with a
//     different call in between, gives two DIFFERENT answers. That is
//     the sharpest available statement of "the result depends on a prior
//     call": it cannot be satisfied by a pure function of the arguments,
//     however elaborate.
//
// Both are checked with the module driven directly, so a regression in
// the replayer cannot mask a regression in the corpus.
func TestVerifyTheCorpusModulesCarryDwarfAndState(t *testing.T) {
	require.True(t, len(parityCorpus) >= 4,
		"the M45 corpus must keep at least four modules; got %d", len(parityCorpus))

	for _, m := range parityCorpus {
		t.Run(m.name, func(t *testing.T) {
			// --- (1) the module produces real trace content -----------
			tmp := t.TempDir()
			out := filepath.Join(tmp, "traces")
			exitCode, _, stderr := runMain(t, "", []string{
				"run", "--boundary-log=" + m.recordingPath(),
				"--out-dir=" + out, m.wasmPath()})
			require.Equal(t, 0, exitCode,
				"replaying %s failed; stderr:\n%s", m.name, stderr)

			candidates, err := filepath.Glob(filepath.Join(out, "*.ct"))
			require.NoError(t, err)
			require.Equal(t, 1, len(candidates),
				"%s: expected exactly one .ct; got %v", m.name, candidates)
			sizes := internalFileSizes(t, candidates[0])
			for _, stream := range corpusTraceStreams {
				size, ok := sizes[stream]
				require.True(t, ok,
					"%s: the container carries no %s at all; the corpus module has "+
						"lost its DWARF", m.name, stream)
				require.True(t, size > 0,
					"%s: %s is %d bytes. A module whose trace streams are empty pins "+
						"only container scaffolding — that is the `grow_mem` hole M45 "+
						"exists to close", m.name, stream, size)
			}

			// --- (2) the export's result depends on a prior call ------
			require.Equal(t, 3, len(m.guard),
				"%s: the guard sequence must be [a, b, a]", m.name)
			require.Equal(t, m.guard[0], m.guard[2],
				"%s: the guard's first and third calls must use the same argument, "+
					"or a difference between their results proves nothing", m.name)

			replayed := m
			replayed.calls = m.guard
			answers := recordDirectCalls(t, replayed, filepath.Join(tmp, "guard"))
			require.Equal(t, 3, len(answers))
			require.False(t, equalResults(answers[0], answers[2]),
				"%s: %s%v answered %v both times. The corpus module no longer carries "+
					"state across calls, so a parity or byte-identity comparison over "+
					"it can no longer distinguish a working entry state from none — "+
					"the `balance_calc` hole M45 exists to close",
				m.name, m.export, m.guard[0], answers[0])
		})
	}
}

func equalResults(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// internalFileSizes reports the byte size of each internal file in a
// `.ct` container, by name.
//
// It reads the container back off disk with `internal/ctfs` rather than
// asking the writer what it wrote, so "the stream is non-empty" is a
// statement about the bytes that landed rather than about an in-memory
// count the writer kept alongside them.
func internalFileSizes(t *testing.T, ctPath string) map[string]int {
	t.Helper()
	container, err := ctfs.Open(ctPath)
	require.NoError(t, err, "opening %s", ctPath)

	sizes := make(map[string]int)
	for _, name := range container.Names() {
		data, err := container.ReadFile(name)
		require.NoError(t, err, "reading %s from %s", name, ctPath)
		sizes[name] = len(data)
	}
	require.True(t, len(sizes) > 0, "%s carries no internal files", ctPath)
	return sizes
}
