package boundarylog

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// hostStateLabel names the spec §3.3 / §3.4 state in diagnostics.
//
// The state is carried by host-state records in the boundary log itself
// (see "The in-stream host-state channel" below):
// `codetracer-wasm-instrumenter/recorder-runtime/browser_session.js` captures
// it (`host_state.js` explains why by snapshot-and-diff from the host side and
// not by a bytecode hook — a host write happens in JavaScript, outside the
// module, where no instruction of the module runs) and emits
// `HostInitialState` / `HostMutation` browser events, which
// `codetracer/src/backend-manager/src/browser_stream_host.rs` renders into
// `Event` records of this schema. A recording whose module defines its own
// memory and globals carries none, and replays exactly as before.
const hostStateLabel = "the recording's host-state records"

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
			hostStateLabel, h.Version, hostStateVersion)
	}
	if len(h.Initial.Tables) > 0 {
		return fmt.Errorf(
			"%s carries imported-table state, which this recorder does not "+
				"replay. Spec §8 lists imported tables mutated by the host among "+
				"the constructs that are refused rather than silently degraded",
			hostStateLabel)
	}
	for _, m := range h.Initial.Memories {
		if m.Name == "" {
			return fmt.Errorf("%s: imported memory entry has no name", hostStateLabel)
		}
		if m.MaxPages != nil && *m.MaxPages < m.MinPages {
			return fmt.Errorf(
				"%s: imported memory %s.%s declares maxPages %d below minPages %d",
				hostStateLabel, m.Module, m.Name, *m.MaxPages, m.MinPages)
		}
		for _, d := range m.Data {
			if _, err := d.decode(); err != nil {
				return fmt.Errorf("%s: imported memory %s.%s: %w",
					hostStateLabel, m.Module, m.Name, err)
			}
		}
	}
	for _, g := range h.Initial.Globals {
		if g.Name == "" {
			return fmt.Errorf("%s: imported global entry has no name", hostStateLabel)
		}
		if _, _, err := g.decode(); err != nil {
			return fmt.Errorf("%s: %w", hostStateLabel, err)
		}
	}
	for _, mu := range h.Mutations {
		if mu.AfterCrossing < 0 {
			return fmt.Errorf("%s: mutation anchored to negative crossing %d",
				hostStateLabel, mu.AfterCrossing)
		}
		for _, w := range mu.MemoryWrites {
			if _, err := w.decode(); err != nil {
				return fmt.Errorf("%s: %w", hostStateLabel, err)
			}
		}
		for _, s := range mu.GlobalSets {
			if _, err := s.decode(); err != nil {
				return fmt.Errorf("%s: %w", hostStateLabel, err)
			}
			if !h.globalIsMutable(s.Module, s.Name) {
				return fmt.Errorf(
					"%s: mutation at crossing %d assigns global %s.%s, which the "+
						"initial state does not declare as a mutable imported global",
					hostStateLabel, mu.AfterCrossing, s.Module, s.Name)
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

// ---------------------------------------------------------------------------
// The in-stream host-state channel (M44b)
// ---------------------------------------------------------------------------
//
// Spec §3.3 state is only known at the module's first exported call, after
// the daemon has opened the stream and spawned its consumer, so it cannot be
// metadata read at startup. It is carried **in the record stream itself**, as
// `Event` records the daemon appends the moment they arrive, and three
// properties are why:
//
//  1. **One ordered stream carries everything.** §3.3 arrives immediately
//     before the first `Call` record and each §3.4 mutation arrives inside
//     the crossing it belongs to, because `browser_session.js` sends them
//     through the same queue as every other event.
//  2. **There is nothing to race.** A record cannot be read before it is
//     written; a side file re-read by the consumer could be.
//  3. **Nothing else has to learn anything.** `Event` is already an open
//     extension point on this path — `parseRealmMarker` returns ok=false
//     for a `boundary_id` it does not know — so `ct print` and the
//     db-backend skip these records in the recording rather than failing on
//     them.

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
	// error (spec §8).
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
func parseHostStateMarker(ev *eventRecord) (*hostStateMarker, bool, error) {
	if ev.Kind != eventKindTraceLogEvent {
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
			// what the daemon does with the same input.
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
