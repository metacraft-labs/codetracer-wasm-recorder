package boundarylog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// CrossingKind distinguishes the two boundary directions the spec records:
// host→WASM (an exported function is called, spec §3.1) and WASM→host (an
// imported function is called, spec §3.2).
type CrossingKind uint8

const (
	// CrossingExport is a call *into* the module — spec §3.1.
	CrossingExport CrossingKind = iota
	// CrossingImport is a call *out of* the module — spec §3.2.
	CrossingImport
)

func (k CrossingKind) String() string {
	if k == CrossingExport {
		return "export"
	}
	return "import"
}

// Crossing is one recorded traversal of the module/host boundary.
//
// Values are held in their recorded `.ct` encoding rather than decoded
// eagerly, because the type of each slot comes from the boundary's
// signature, which is resolved later against the module (see replay.go).
type Crossing struct {
	// Seq is the crossing's position in *call* order across the whole
	// recording, starting at 0. It is the identity replay checks an
	// import against, so a diagnostic can name "the 3rd crossing".
	Seq int
	// Depth is the crossing's nesting depth: 0 for a call the host made
	// directly, greater for one made while another crossing was open.
	Depth int
	// Kind is CrossingExport or CrossingImport.
	Kind CrossingKind
	// Name is the exported function's name for an export crossing, and
	// "" for an import crossing (the browser rendering identifies imports
	// by index only — see the "Import crossings" note below).
	Name string
	// Index is the import index for an import crossing. It is
	// meaningless, and left zero, for an export crossing: the browser
	// rendering identifies exports by name.
	Index uint32
	// Args and Results are the recorded argument and result tuples, in
	// slot order.
	Args    []rawValue
	Results []rawValue
	// hasResults records whether a result run was seen at all, so a
	// crossing that legitimately returns nothing is distinguishable from
	// one whose results the rendering dropped.
	hasResults bool
}

// Describe renders the crossing for a diagnostic.
func (c *Crossing) Describe() string {
	if c.Kind == CrossingExport {
		return fmt.Sprintf("crossing #%d: export %q", c.Seq, c.Name)
	}
	return fmt.Sprintf("crossing #%d: import #%d", c.Seq, c.Index)
}

// Recording is a parsed boundary recording: the crossings the browser
// observed, plus the host-supplied state the spec's §3.3 / §3.4 describe.
type Recording struct {
	// Program is `trace_metadata.json`'s `program` field.
	Program string
	// Workdir is `trace_metadata.json`'s `workdir` field.
	Workdir string
	// Recorder names the producer, e.g. "codetracer-js-recorder-browser".
	Recorder string
	// Paths is `trace_paths.json`.
	Paths []string
	// Crossings are every recovered boundary crossing, in call order.
	Crossings []Crossing
	// HostState carries spec §3.3 initial state and §3.4 mutations when
	// the recording ships a `boundary_state.json` sidecar; nil otherwise.
	HostState *HostState
	// Source is the path the recording was loaded from, for diagnostics.
	Source string
}

// TopLevelExports returns the export crossings the host initiated directly,
// in call order. These are the calls replay drives (spec §6.4); nested
// export crossings are re-entries caused by a host callback and are driven
// by the stub that makes the callback, not by the driver.
func (r *Recording) TopLevelExports() []*Crossing {
	var out []*Crossing
	for i := range r.Crossings {
		if r.Crossings[i].Kind == CrossingExport && r.Crossings[i].Depth == 0 {
			out = append(out, &r.Crossings[i])
		}
	}
	return out
}

// NestedExports returns export crossings that occurred while another
// crossing was open. Replay rejects a recording containing one: driving a
// host callback would need the host's own control flow, which a boundary
// log does not carry. Refusing is the spec §8 discipline — a recording that
// cannot be faithfully replayed is rejected, not silently degraded.
func (r *Recording) NestedExports() []*Crossing {
	var out []*Crossing
	for i := range r.Crossings {
		if r.Crossings[i].Kind == CrossingExport && r.Crossings[i].Depth > 0 {
			out = append(out, &r.Crossings[i])
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// `.ct` (three-file JSON) decoding
// ---------------------------------------------------------------------------

// traceMetadata mirrors `trace_metadata.json`, written by
// `browser_stream_host.rs::JsonFileCtfsWriter::flush`.
type traceMetadata struct {
	Program  string   `json:"program"`
	Args     []string `json:"args"`
	Workdir  string   `json:"workdir"`
	Recorder struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"recorder"`
}

// traceEvent is one element of `trace.json`. The Rust producer serialises
// `TraceLowLevelEvent` as an externally tagged enum with PascalCase names,
// so each element is a single-key object.
type traceEvent struct {
	Path         *string          `json:"Path"`
	Step         *json.RawMessage `json:"Step"`
	Function     *functionRecord  `json:"Function"`
	Call         *callRecord      `json:"Call"`
	Return       *returnRecord    `json:"Return"`
	Value        *valueRecord     `json:"Value"`
	VariableName *string          `json:"VariableName"`
	Event        *json.RawMessage `json:"Event"`
}

type functionRecord struct {
	Name   string `json:"name"`
	PathID uint32 `json:"path_id"`
	Line   int64  `json:"line"`
}

type callRecord struct {
	FunctionID uint32 `json:"function_id"`
	// Args is always empty on the browser path — `browser_session.js`
	// sends `{kind:"Call", fnId, args: []}` and delivers the arguments as
	// separate `Value` records. Decoded anyway so a producer that starts
	// populating it does not silently lose data.
	Args []json.RawMessage `json:"args"`
}

type returnRecord struct {
	ReturnValue json.RawMessage `json:"return_value"`
}

type valueRecord struct {
	VariableID uint32          `json:"variable_id"`
	Value      json.RawMessage `json:"value"`
}

// onDiskValue is `ValueRecordOnDisk`: an internally tagged enum whose
// payload field name depends on the variant.
type onDiskValue struct {
	Kind string `json:"kind"`
	I    string `json:"i"`
	F    string `json:"f"`
	R    string `json:"r"`
	Text string `json:"text"`
	B    *bool  `json:"b"`
}

func (v onDiskValue) raw() rawValue {
	switch v.Kind {
	case "Int":
		return rawValue{Kind: v.Kind, Text: v.I}
	case "Float":
		return rawValue{Kind: v.Kind, Text: v.F}
	case "Raw":
		return rawValue{Kind: v.Kind, Text: v.R}
	case "String":
		return rawValue{Kind: v.Kind, Text: v.Text}
	case "Bool":
		if v.B != nil && *v.B {
			return rawValue{Kind: v.Kind, Text: "true"}
		}
		return rawValue{Kind: v.Kind, Text: "false"}
	default:
		return rawValue{Kind: v.Kind}
	}
}

// LoadRecording reads a boundary recording from `path`, which may be either
// the `.ct` directory itself or its `trace.json`.
func LoadRecording(path string) (*Recording, error) {
	rec, err := LoadRecordingMetadata(path)
	if err != nil {
		return nil, err
	}
	dir := rec.Source

	eventsRaw, err := os.ReadFile(filepath.Join(dir, "trace.json"))
	if err != nil {
		return nil, fmt.Errorf("reading boundary recording events: %w", err)
	}
	var events []traceEvent
	if err := json.Unmarshal(eventsRaw, &events); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", filepath.Join(dir, "trace.json"), err)
	}

	crossings, err := reconstructCrossings(events)
	if err != nil {
		return nil, fmt.Errorf("recovering boundary crossings from %s: %w", dir, err)
	}
	rec.Crossings = crossings
	return rec, nil
}

// LoadRecordingMetadata reads everything about a recording *except* its
// crossings: the program metadata, the source paths and the spec §3.3 / §3.4
// host state.
//
// This is what a streaming replay starts from
// (`WASM-Replay-Snapshots-And-Slices.md` §2). None of these files describes the
// execution, so none of them has to have arrived before the first exported call
// can be driven — and on the browser path they typically have not: the daemon's
// `JsonFileCtfsWriter` writes them at flush time, after `trace.json` has been
// growing for a while. Every one of them is therefore optional, exactly as it
// already is for a finished recording.
func LoadRecordingMetadata(path string) (*Recording, error) {
	dir, err := recordingDir(path)
	if err != nil {
		return nil, err
	}

	rec := &Recording{Source: dir}

	// trace_metadata.json and trace_paths.json are informational: a
	// recording without them still replays, so a missing file is not
	// fatal, but a malformed one is (it means the recording is damaged).
	metaPath := filepath.Join(dir, "trace_metadata.json")
	if metaRaw, err := os.ReadFile(metaPath); err == nil {
		var meta traceMetadata
		if err := json.Unmarshal(metaRaw, &meta); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", metaPath, err)
		}
		rec.Program = meta.Program
		rec.Workdir = meta.Workdir
		rec.Recorder = meta.Recorder.Name
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", metaPath, err)
	}

	pathsPath := filepath.Join(dir, "trace_paths.json")
	if pathsRaw, err := os.ReadFile(pathsPath); err == nil {
		if err := json.Unmarshal(pathsRaw, &rec.Paths); err != nil {
			return nil, fmt.Errorf("decoding %s: %w", pathsPath, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("reading %s: %w", pathsPath, err)
	}

	state, err := loadHostState(dir)
	if err != nil {
		return nil, err
	}
	rec.HostState = state

	return rec, nil
}

// recordingDir normalises a user-supplied `--boundary-log` argument to the
// directory holding the three trace files.
func recordingDir(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("opening boundary recording %s: %w", path, err)
	}
	if st.IsDir() {
		return path, nil
	}
	if filepath.Base(path) != "trace.json" {
		return "", fmt.Errorf(
			"boundary recording %s is neither a `.ct` directory nor a `trace.json`; "+
				"pass the `<program>.ct` directory the CodeTracer backend-manager wrote", path)
	}
	return filepath.Dir(path), nil
}

// ---------------------------------------------------------------------------
// Crossing reconstruction
// ---------------------------------------------------------------------------
//
// # How a crossing appears in a browser `.ct`
//
// `browser_session.js` renders the hook stream into the JS recorder's
// vocabulary before it ever reaches disk, so the boundary structure has to
// be recovered from that rendering rather than read off directly. The rules
// it emits by (see the `__ct_emit_*` handlers and `flushValues`) are:
//
//   - Every value hook appends to a pending run. The run is flushed — as a
//     `Step` followed by one `VariableName`+`Value` pair per slot — when the
//     next `__ct_emit_call`, realm marker or `__ct_emit_return` arrives.
//   - Each binding is named `<label>:<role><slot>` where role is `arg` or
//     `ret`, and label is the export's name (from the manifest) for an
//     export crossing, or the literal `import #<n>` for an import crossing.
//   - An **export** crossing additionally emits `Call{fnId}` before its
//     argument run and `Return{returnValue}` after its result run. So an
//     export crossing is bracketed on disk even when it has no values.
//   - An **import** crossing emits *no* `Call` and *no* `Return` —
//     `browser_session.js` returns early for any `fnKind` that is not
//     `FUNC_KIND_EXPORT`. It DOES emit the same pair of correlation
//     markers an export does: `replace_imported_call` in the instrumenter
//     pushes `push_realm_boundary(..., FUNC_KIND_IMPORT, ...)` on both
//     sides of the edge, and `__ct_emit_realm_boundary` ignores its
//     `fnKind` argument. Those markers carry no boundary values, so the
//     import's structure still has to be recovered from its value runs.
//
// The reconstruction below therefore brackets exports on `Call`/`Return`
// and brackets imports on their argument/result runs. Two consequences are
// inherent to the *producer* and are reported rather than guessed at:
//
//  1. An import whose signature is `() -> ()` contributes no value runs,
//     so this parser does not recover it. Its index is not absent from the
//     recording — the markers name it — but the marker label uses the same
//     `wasm export #<n>` template for both edges, so a marker cannot be
//     attributed to an import by its own content. Recovering these
//     crossings needs the producer to spell the two edges differently;
//     until then replay reports them (`Result.UncheckedImportCalls`).
//  2. Two adjacent import crossings of the same import, the first with
//     arguments but no results, are indistinguishable from one crossing
//     whose argument run was recorded twice. `reconstructCrossings` closes
//     the open crossing when a second argument run for the same import
//     arrives, which is the reading that keeps call counts right.

// valueRun is a maximal consecutive group of `Value` records sharing one
// `(label, role)` pair.
type valueRun struct {
	label  string
	role   string // "arg" or "ret"
	values []rawValue
}

// openCrossing is a crossing whose start has been seen but whose end has
// not.
type openCrossing struct {
	idx   int // index into the result slice
	label string
}

func reconstructCrossings(events []traceEvent) ([]Crossing, error) {
	a := newAssembler()
	for i := range events {
		if err := a.push(&events[i]); err != nil {
			return nil, err
		}
	}
	if err := a.finish(); err != nil {
		return nil, err
	}
	return a.crossings, nil
}

// parseBindingName splits `browser_session.js`'s `boundaryBindingName`
// output — “ `${label}:${role}${slot}` “ — back into its parts.
//
// The label may itself contain a colon (`import #3` does not, but an
// exported function name is arbitrary), so the split is anchored on the
// LAST colon and the suffix must be exactly `arg<digits>` or `ret<digits>`.
func parseBindingName(name string) (label, role string, slot int, ok bool) {
	i := strings.LastIndexByte(name, ':')
	if i < 0 || i == len(name)-1 {
		return "", "", 0, false
	}
	label, suffix := name[:i], name[i+1:]
	switch {
	case strings.HasPrefix(suffix, "arg"):
		role, suffix = "arg", suffix[3:]
	case strings.HasPrefix(suffix, "ret"):
		role, suffix = "ret", suffix[3:]
	default:
		return "", "", 0, false
	}
	if suffix == "" {
		return "", "", 0, false
	}
	n, err := strconv.Atoi(suffix)
	if err != nil || n < 0 {
		return "", "", 0, false
	}
	if label == "" {
		return "", "", 0, false
	}
	return label, role, n, true
}

// importLabelPrefix is the label `browser_session.js` gives an import
// crossing's value bindings: “ `import #${fnIndex}` “.
const importLabelPrefix = "import #"

// parseImportLabel reports whether a binding label denotes an import
// crossing and, if so, which import index.
func parseImportLabel(label string) (uint32, bool) {
	if !strings.HasPrefix(label, importLabelPrefix) {
		return 0, false
	}
	n, err := strconv.ParseUint(label[len(importLabelPrefix):], 10, 32)
	if err != nil {
		return 0, false
	}
	return uint32(n), true
}
