package boundarylog

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// HostStateFileName is the sidecar, inside the `.ct` directory, that
// carries the spec §3.3 host-supplied initial state and the spec §3.4 host
// mutations. It is optional: a module that defines its own memory and
// globals needs none of it, because the `.wasm` already contains them.
//
// PRODUCER STATUS: as of M44 this file **is** produced by the browser
// pipeline. `codetracer-wasm-instrumenter/recorder-runtime/browser_session.js`
// captures the state (`host_state.js` explains why by snapshot-and-diff from
// the host side and not by a bytecode hook — a host write happens in
// JavaScript, outside the module, where no instruction of the module runs),
// emits it as `HostInitialState` / `HostMutation` browser events, and
// `codetracer/src/backend-manager/src/browser_stream_host.rs` renders those
// into this schema. A recording whose module defines its own memory and
// globals still carries no such file, and replays exactly as before.
//
// The end-to-end fixture is
// `codetracer/src/db-backend/tests/fixtures/wasm-memory-calldata/`, whose
// `verify.sh` replays a real browser recording and shows that withholding
// either record produces a `DivergenceError` rather than a wrong trace.
//
// Two limits of the producer, both reported at the cause rather than
// dropped (spec §8), and neither of them this package's to fix:
//
//   - A host write made *between* two top-level exported calls has no
//     anchor in this schema — §3.3 is "before the FIRST call" and §3.4 is
//     "during crossing N". The producer refuses to invent one. This covers
//     an imported global the host reassigns between calls as well as a
//     memory write: `applyMutations` only ever sets a provider global from
//     a mutation, so an assignment anchored to neither record would simply
//     never be applied.
//   - A `() -> ()` import leaves no value run, so no crossing is recovered
//     for it (see `recording.go`) and a mutation made during it has no
//     `AfterCrossing` that would be true. Likewise refused.
//
// A third case is refused *here* rather than by the producer, and it is
// what makes the producer's capture windows exact. §3.4's window is the
// span between an import's two hooks, on the reasoning that only the host
// runs inside it. The one way a module store can land there is a host
// function calling back into an exported function — and such a recording
// carries an export crossing at non-zero depth, which `refuseNestedExports`
// rejects on both the batch and the streaming path. So a host write and a
// module write to overlapping addresses cannot reach a materialised trace:
// the recording is refused, not mis-attributed.
const HostStateFileName = "boundary_state.json"

// hostStateVersion is the schema version this package understands. An
// unrecognised version is a hard error rather than a best-effort read: the
// whole point of §3.3 is that a missing input produces a divergence later,
// at a point unrelated to the cause (spec §8).
const hostStateVersion = 1

// HostState is the decoded `boundary_state.json`.
type HostState struct {
	// Version must equal hostStateVersion.
	Version int `json:"version"`
	// Initial is the state the host had supplied before the first
	// exported call (spec §3.3).
	Initial InitialState `json:"initial"`
	// Mutations are writes the host made while servicing an imported call
	// (spec §3.4), each anchored to the crossing it accompanied.
	Mutations []HostMutation `json:"mutations"`

	// initialSeen records that an §3.3 record has arrived, which is what
	// distinguishes "the host supplied nothing" from "the host supplied an
	// empty set of regions". Only the in-stream channel needs it — the
	// sidecar is written whole, so its presence is the same statement —
	// and it is unexported so it never reaches the wire.
	initialSeen bool `json:"-"`
}

// InitialState is spec §3.3: anything the module imports rather than
// defines.
type InitialState struct {
	Memories []ImportedMemory `json:"memories"`
	Globals  []ImportedGlobal `json:"globals"`
	// Tables is decoded so a recording that carries imported-table state
	// is REJECTED rather than silently replayed without it. Spec §8 lists
	// "imported tables mutated by the host during execution" among the
	// constructs that are refused instead of degraded.
	Tables []json.RawMessage `json:"tables"`
}

// ImportedMemory describes one memory the module imports.
type ImportedMemory struct {
	// Module and Name are the import's two-level name.
	Module string `json:"module"`
	Name   string `json:"name"`
	// MinPages is the memory's size in 64 KiB pages at the moment the
	// recording started. Spec §7 notes `memory.grow`'s result depends on
	// host limits and is therefore recorded as part of the initial state:
	// MaxPages carries that limit.
	MinPages uint32 `json:"minPages"`
	// MaxPages is the declared maximum, or nil for "unbounded".
	MaxPages *uint32 `json:"maxPages"`
	// Data are the non-zero regions of the memory's initial contents.
	Data []MemoryRegion `json:"data"`
}

// MemoryRegion is a run of bytes at an absolute offset in linear memory.
type MemoryRegion struct {
	Offset uint32 `json:"offset"`
	// BytesB64 is the region's contents, base64-encoded (standard
	// alphabet with padding). Base64 rather than hex because an initial
	// memory image is large and this file sits inside the trace bundle.
	BytesB64 string `json:"bytesB64"`
}

// decode returns the region's bytes.
func (m MemoryRegion) decode() ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(m.BytesB64)
	if err != nil {
		return nil, fmt.Errorf("memory region at offset %d: invalid base64: %w", m.Offset, err)
	}
	return b, nil
}

// ImportedGlobal describes one global the module imports.
type ImportedGlobal struct {
	Module string `json:"module"`
	Name   string `json:"name"`
	// Type is a lowercase scalar type spelling ("i32", "i64", "f32", "f64").
	Type string `json:"type"`
	// Mutable declares whether the module may write the global — and
	// therefore whether a §3.4 mutation may target it.
	Mutable bool `json:"mutable"`
	// Value is the initial value, encoded exactly like a recorded
	// boundary value: a decimal string for the integer types, a decimal
	// float for the float types.
	Value string `json:"value"`
}

// decode returns the global's declared type and initial value.
func (g ImportedGlobal) decode() (ScalarType, Value, error) {
	t, err := ParseScalarType(g.Type)
	if err != nil {
		return 0, Value{}, fmt.Errorf("imported global %s.%s: %w", g.Module, g.Name, err)
	}
	kind := "Int"
	if t == TypeF32 || t == TypeF64 {
		kind = "Float"
	}
	v, err := rawValue{Kind: kind, Text: g.Value}.decode(t)
	if err != nil {
		return 0, Value{}, fmt.Errorf("imported global %s.%s: %w", g.Module, g.Name, err)
	}
	return t, v, nil
}

// HostMutation is spec §3.4: a write the host made to imported memory or an
// imported global while servicing an imported call. It is part of the
// call's observable result, so it is applied at exactly the recorded point.
type HostMutation struct {
	// AfterCrossing is the `Crossing.Seq` of the crossing this mutation
	// accompanied. The mutation is applied when that crossing's stub is
	// invoked, *before* the recorded results are handed back — which is
	// the order the module observes: it sees the memory the host wrote
	// and the value the host returned as one indivisible outcome.
	AfterCrossing int `json:"afterCrossing"`
	// MemoryWrites are byte ranges the host wrote.
	MemoryWrites []MemoryWrite `json:"memoryWrites"`
	// GlobalSets are imported globals the host assigned.
	GlobalSets []GlobalSet `json:"globalSets"`
}

// MemoryWrite is one host write into an imported memory.
type MemoryWrite struct {
	Module   string `json:"module"`
	Name     string `json:"name"`
	Offset   uint32 `json:"offset"`
	BytesB64 string `json:"bytesB64"`
}

func (w MemoryWrite) decode() ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(w.BytesB64)
	if err != nil {
		return nil, fmt.Errorf("memory write to %s.%s at offset %d: invalid base64: %w",
			w.Module, w.Name, w.Offset, err)
	}
	return b, nil
}

// GlobalSet is one host assignment to an imported mutable global.
type GlobalSet struct {
	Module string `json:"module"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Value  string `json:"value"`
}

func (s GlobalSet) decode() (Value, error) {
	t, err := ParseScalarType(s.Type)
	if err != nil {
		return Value{}, fmt.Errorf("global set %s.%s: %w", s.Module, s.Name, err)
	}
	kind := "Int"
	if t == TypeF32 || t == TypeF64 {
		kind = "Float"
	}
	v, err := rawValue{Kind: kind, Text: s.Value}.decode(t)
	if err != nil {
		return Value{}, fmt.Errorf("global set %s.%s: %w", s.Module, s.Name, err)
	}
	return v, nil
}

// MutationsFor returns the mutations anchored to one crossing.
func (h *HostState) MutationsFor(seq int) []HostMutation {
	if h == nil {
		return nil
	}
	var out []HostMutation
	for i := range h.Mutations {
		if h.Mutations[i].AfterCrossing == seq {
			out = append(out, h.Mutations[i])
		}
	}
	return out
}

// providerModules groups the initial state by the module name the guest
// imports it from, so one synthesised provider module can be built per
// name. Returns the names in deterministic (sorted) order.
func (h *HostState) providerModules() ([]string, map[string]*providerSpec) {
	specs := map[string]*providerSpec{}
	get := func(name string) *providerSpec {
		s, ok := specs[name]
		if !ok {
			s = &providerSpec{module: name}
			specs[name] = s
		}
		return s
	}
	if h != nil {
		for _, m := range h.Initial.Memories {
			get(m.Module).memories = append(get(m.Module).memories, m)
		}
		for _, g := range h.Initial.Globals {
			get(g.Module).globals = append(get(g.Module).globals, g)
		}
	}
	names := make([]string, 0, len(specs))
	for n := range specs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, specs
}

// validate rejects a host state this package cannot faithfully apply,
// rather than applying the part it understands (spec §8).
func (h *HostState) validate() error {
	if h.Version != hostStateVersion {
		return fmt.Errorf(
			"%s declares version %d but this recorder implements version %d; "+
				"refusing to guess at an unknown schema (spec §8)",
			HostStateFileName, h.Version, hostStateVersion)
	}
	if len(h.Initial.Tables) > 0 {
		return fmt.Errorf(
			"%s carries imported-table state, which this recorder does not "+
				"replay. Spec §8 lists imported tables mutated by the host among "+
				"the constructs that are refused rather than silently degraded",
			HostStateFileName)
	}
	for _, m := range h.Initial.Memories {
		if m.Name == "" {
			return fmt.Errorf("%s: imported memory entry has no name", HostStateFileName)
		}
		if m.MaxPages != nil && *m.MaxPages < m.MinPages {
			return fmt.Errorf(
				"%s: imported memory %s.%s declares maxPages %d below minPages %d",
				HostStateFileName, m.Module, m.Name, *m.MaxPages, m.MinPages)
		}
		for _, d := range m.Data {
			if _, err := d.decode(); err != nil {
				return fmt.Errorf("%s: imported memory %s.%s: %w",
					HostStateFileName, m.Module, m.Name, err)
			}
		}
	}
	for _, g := range h.Initial.Globals {
		if g.Name == "" {
			return fmt.Errorf("%s: imported global entry has no name", HostStateFileName)
		}
		if _, _, err := g.decode(); err != nil {
			return fmt.Errorf("%s: %w", HostStateFileName, err)
		}
	}
	for _, mu := range h.Mutations {
		if mu.AfterCrossing < 0 {
			return fmt.Errorf("%s: mutation anchored to negative crossing %d",
				HostStateFileName, mu.AfterCrossing)
		}
		for _, w := range mu.MemoryWrites {
			if _, err := w.decode(); err != nil {
				return fmt.Errorf("%s: %w", HostStateFileName, err)
			}
		}
		for _, s := range mu.GlobalSets {
			if _, err := s.decode(); err != nil {
				return fmt.Errorf("%s: %w", HostStateFileName, err)
			}
			if !h.globalIsMutable(s.Module, s.Name) {
				return fmt.Errorf(
					"%s: mutation at crossing %d assigns global %s.%s, which the "+
						"initial state does not declare as a mutable imported global",
					HostStateFileName, mu.AfterCrossing, s.Module, s.Name)
			}
		}
	}
	return nil
}

func (h *HostState) globalIsMutable(module, name string) bool {
	for _, g := range h.Initial.Globals {
		if g.Module == module && g.Name == name {
			return g.Mutable
		}
	}
	return false
}

// normalise replaces every nil slice with an empty one.
//
// It exists for `reconcileHostState`, which compares two independently
// built descriptions of one document by their JSON rendering: `nil`
// renders as `null` and an empty slice as `[]`, and the two carriers
// disagree about which they produce for an absent list purely because one
// is decoded whole and the other accumulated. Nothing downstream can tell
// the two apart — every consumer of these fields ranges over them.
func (h *HostState) normalise() {
	if h == nil {
		return
	}
	if h.Mutations == nil {
		h.Mutations = []HostMutation{}
	}
	if h.Initial.Memories == nil {
		h.Initial.Memories = []ImportedMemory{}
	}
	if h.Initial.Globals == nil {
		h.Initial.Globals = []ImportedGlobal{}
	}
	if h.Initial.Tables == nil {
		h.Initial.Tables = []json.RawMessage{}
	}
	for i := range h.Initial.Memories {
		if h.Initial.Memories[i].Data == nil {
			h.Initial.Memories[i].Data = []MemoryRegion{}
		}
	}
	for i := range h.Mutations {
		if h.Mutations[i].MemoryWrites == nil {
			h.Mutations[i].MemoryWrites = []MemoryWrite{}
		}
		if h.Mutations[i].GlobalSets == nil {
			h.Mutations[i].GlobalSets = []GlobalSet{}
		}
	}
}

// loadHostState reads the optional `boundary_state.json` sidecar from a
// recording directory. A missing file yields (nil, nil); a malformed or
// unsupported one is an error.
func loadHostState(dir string) (*HostState, error) {
	path := filepath.Join(dir, HostStateFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var h HostState
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", path, err)
	}
	if err := h.validate(); err != nil {
		return nil, err
	}
	return &h, nil
}

// ---------------------------------------------------------------------------
// The in-stream host-state channel (M44b)
// ---------------------------------------------------------------------------
//
// The sidecar above is a *file*, and a file is exactly what a streaming
// consumer cannot use. `--boundary-stream` opens the recording while the
// page is still running, and spec §3.3 state is only known at the module's
// first exported call — which happens after the daemon has opened the
// stream and spawned its consumer. `LoadRecordingMetadata` runs once, at
// startup, so on that path the sidecar is reliably absent and
// `checkImportedMemories` refuses the recording: every Stylus contract and
// every `wasm-bindgen` glue layer was excluded from the streaming pipeline.
//
// M44b closes that by carrying the same two records **in the event stream
// itself**, as `Event` records the daemon appends to `trace.json` at the
// moment they arrive. That is the milestone's preferred shape, and three
// properties are why:
//
//  1. **One ordered stream carries everything.** A sidecar the consumer
//     re-reads reintroduces exactly the ordering ambiguity the stream
//     exists to remove: nothing would relate "the file changed" to "the
//     crossing it describes". Here §3.3 arrives immediately before the
//     first `Call` record and each §3.4 mutation arrives inside the
//     crossing it belongs to, because `browser_session.js` sends them
//     through the same queue as every other event.
//  2. **A re-read protocol would be racy, not merely late.** The daemon's
//     `write_host_state` calls `open_stream`, and `open_stream` is what
//     spawns the §2 consumer — so the consumer's first read races the
//     sidecar's first write. In-stream carriage has no such window: the
//     record cannot be read before it is written.
//  3. **Nothing else has to learn anything.** `Event` is already an open
//     extension point on this path — `parseRealmMarker` returns ok=false
//     for a `boundary_id` it does not know, and the JS recorder already
//     puts HTTP and other domain markers into its recordings — so an
//     older `wazero`, `ct-print` and the db-backend all skip these
//     records rather than failing on them.
//
// The sidecar is still written, unchanged, and is still what a *batch*
// replay of an older recording reads. It is now a rendering of the stream
// rather than the only copy, and `LoadRecording` cross-checks the two when
// a recording carries both — a producer that let them disagree would be
// serving two different programs to the two drivers.

// hostStateBoundary is the `boundary_id` under which the daemon marks a
// host-state record. It is deliberately not `js-wasm-realm`: a reader of
// the realm markers must not have to distinguish these, and this package's
// own `parseRealmMarker` rejects it on the `boundary_id` check alone.
//
// Must match `HOST_STATE_BOUNDARY_ID` in
// `codetracer/src/backend-manager/src/browser_stream_host.rs`.
const hostStateBoundary = "wasm-host-state"

// The two record kinds the channel carries.
const (
	hostStateRecordInitial  = "initial"
	hostStateRecordMutation = "mutation"
)

// hostStateMarker is one decoded in-stream host-state record.
//
// It is the nested `metadata` document of an `Event`, in the same shape
// the realm markers use — a JSON object serialised into the `metadata`
// string — because that is the only field of `RecordEvent` a producer can
// put structure into without inventing a record type every existing reader
// would have to learn.
type hostStateMarker struct {
	BoundaryID string `json:"boundary_id"`
	// Version mirrors `HostState.Version`; an unrecognised one is a hard
	// error for the same reason it is in the sidecar.
	Version int    `json:"version"`
	Record  string `json:"record"`
	// Initial is set for a `hostStateRecordInitial` record.
	Initial *InitialState `json:"initial"`
	// Mutation is set for a `hostStateRecordMutation` record.
	Mutation *HostMutation `json:"mutation"`
}

// parseHostStateMarker decodes one `Event` record as a host-state record.
//
// ok=false means "this is not one", which covers every other `Event` on
// the path and is quiet by design. A non-nil error means "this IS one and
// it cannot be applied" — an unknown schema version, or a record naming
// neither kind. That asymmetry is spec §8: an unrecognised *extension* is
// skipped, but a recognised input that cannot be honoured is refused
// rather than dropped, because dropping it would produce a divergence
// later, at a point unrelated to the cause.
func parseHostStateMarker(raw json.RawMessage) (*hostStateMarker, bool, error) {
	var ev eventRecord
	if err := json.Unmarshal(raw, &ev); err != nil || ev.Kind != eventKindTraceLogEvent {
		return nil, false, nil
	}
	// Cheap pre-filter so the common case — a realm marker, or a domain
	// marker from another recorder on the page — costs one substring
	// search rather than a full decode of a document that is not ours.
	if !strings.Contains(ev.Metadata, hostStateBoundary) {
		return nil, false, nil
	}
	var m hostStateMarker
	if err := json.Unmarshal([]byte(ev.Metadata), &m); err != nil {
		return nil, false, nil
	}
	if m.BoundaryID != hostStateBoundary {
		return nil, false, nil
	}
	if m.Version != hostStateVersion {
		return nil, true, fmt.Errorf(
			"the boundary stream carries a %q record of version %d but this "+
				"recorder implements version %d; refusing to guess at an unknown "+
				"schema (spec §8)", hostStateBoundary, m.Version, hostStateVersion)
	}
	switch m.Record {
	case hostStateRecordInitial:
		if m.Initial == nil {
			return nil, true, fmt.Errorf(
				"the boundary stream carries a %q initial-state record with no "+
					"`initial` document", hostStateBoundary)
		}
	case hostStateRecordMutation:
		if m.Mutation == nil {
			return nil, true, fmt.Errorf(
				"the boundary stream carries a %q mutation record with no "+
					"`mutation` document", hostStateBoundary)
		}
	default:
		return nil, true, fmt.Errorf(
			"the boundary stream carries a %q record of unknown kind %q",
			hostStateBoundary, m.Record)
	}
	return &m, true, nil
}

// foldHostStateMarker applies one decoded record to the accumulating
// state, creating it on first sight.
//
// The accumulated value is validated after every record rather than once
// at the end, because a streaming replay acts on it as it arrives: by the
// time the stream is over, §3.3 has already been applied and several §3.4
// mutations have already been written into linear memory. Validating late
// would mean refusing a recording the replay had already believed.
func foldHostStateMarker(state *HostState, m *hostStateMarker) (*HostState, error) {
	if state == nil {
		state = &HostState{Version: hostStateVersion}
	}
	switch m.Record {
	case hostStateRecordInitial:
		if state.initialSeen {
			// The producer emits this once, immediately before the first
			// exported call. A second one would mean two recordings were
			// spliced together; keeping the first is the only reading
			// that stays true to the calls already replayed, and it is
			// what the daemon's sidecar does with the same input.
			return state, nil
		}
		state.initialSeen = true
		state.Initial = *m.Initial
	case hostStateRecordMutation:
		state.Mutations = append(state.Mutations, *m.Mutation)
	}
	if err := state.validate(); err != nil {
		return nil, err
	}
	return state, nil
}

// reconcileHostState decides which of a recording's two possible carriers
// of spec §3.3 / §3.4 state to believe, and refuses a recording whose two
// carriers disagree.
//
//   - Only the sidecar: a recording made before M44b, or one replayed in
//     batch. Believed as it always was.
//   - Only the stream: a recording still being produced, whose sidecar has
//     not been written yet — or one produced by a daemon that writes only
//     the stream.
//   - Both: they must agree. The sidecar IS a rendering of the stream, so
//     a difference means a producer bug, and the two drivers would
//     otherwise replay two different programs from one recording.
func reconcileHostState(sidecar, streamed *HostState) (*HostState, error) {
	switch {
	case streamed == nil:
		return sidecar, nil
	case sidecar == nil:
		return streamed, nil
	}
	// Both carriers describe the same document but reach it differently —
	// the sidecar is decoded whole, the stream is accumulated record by
	// record — so an absent list is a nil slice on one side and an empty
	// one on the other. `normalise` makes that difference unrepresentable
	// before the comparison, so the comparison is about content.
	sidecar.normalise()
	streamed.normalise()
	a, err := json.Marshal(sidecar)
	if err != nil {
		return nil, fmt.Errorf("re-encoding the %s sidecar: %w", HostStateFileName, err)
	}
	b, err := json.Marshal(streamed)
	if err != nil {
		return nil, fmt.Errorf("re-encoding the streamed host state: %w", err)
	}
	if !bytes.Equal(a, b) {
		return nil, fmt.Errorf(
			"the recording's %s disagrees with the host-state records carried in "+
				"its event stream. The sidecar is a rendering of the stream, so a "+
				"difference means the producer wrote two descriptions of one "+
				"program; refusing rather than picking one (spec §8).\n"+
				"  sidecar: %s\n  stream:  %s",
			HostStateFileName, a, b)
	}
	return sidecar, nil
}
