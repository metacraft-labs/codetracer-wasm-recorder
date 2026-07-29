package boundarylog

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// HostStateFileName is the sidecar, inside the `.ct` directory, that
// carries the spec §3.3 host-supplied initial state and the spec §3.4 host
// mutations. It is optional: a module that defines its own memory and
// globals needs none of it, because the `.wasm` already contains them.
//
// PRODUCER STATUS, stated plainly so nobody mistakes this for something the
// browser writes today: as of M37 **no producer emits this file**. The
// browser recorder (`browser_session.js`) records only the hook stream, and
// the backend-manager receiver (`browser_stream_host.rs`) writes only the
// three trace files. This schema and its application are implemented here
// so that (a) the replay side of spec §3.3/§3.4 exists and is tested, and
// (b) a producer has a defined target to write. A recording without the
// file replays exactly as before.
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
