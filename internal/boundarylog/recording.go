package boundarylog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/tetratelabs/wazero/internal/ctfs"
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
	// markerBracketed records that this crossing was delimited by its own
	// pair of `wasm import #<n>` realm markers rather than only by its
	// value runs (M39). It is the whole reason a crossing whose signature
	// is `() -> ()` can be recovered at all — such a crossing has no value
	// runs — and it doubles as the recording's format witness: a producer
	// older than M39 spelled both edges `wasm export #<n>`, so it never
	// sets this. See `Recording.MarkersIdentifyImports`.
	markerBracketed bool
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
	// Program, Args and Workdir are the boundary log's Header frame.
	Program string
	Args    []string
	Workdir string
	// Recorder names the producer, e.g. "codetracer-js-recorder-browser".
	Recorder string
	// Paths are the recording's source paths, in `Path`-record order: a
	// path's position is the `path_id` every step refers to.
	Paths []string
	// Crossings are every recovered boundary crossing, in call order.
	Crossings []Crossing
	// HostState carries the spec §3.3 initial state and §3.4 mutations the
	// recording's host-state records describe; nil when it has none.
	HostState *HostState
	// MarkersIdentifyImports reports that this recording's realm markers
	// name the import edge in their own right (`wasm import #<n>`), which
	// is what M39 added to `browser_session.js`.
	//
	// It is the recording's producer-version witness, and it decides one
	// thing: whether a call to an import with an empty `() -> ()`
	// signature that replay cannot match is a **divergence** or an
	// unchecked call.
	//
	//   - true — every import crossing is on disk in a form this parser
	//     recovers, including the value-less ones. A call that does not
	//     match the recording is a spec §6 divergence, like any other.
	//   - false — the recording predates M39 (or carries no import
	//     crossing at all, which is indistinguishable from it). A
	//     value-less crossing left only `wasm export #<n>` markers, which
	//     cannot be attributed to an import, so such a call is replayed
	//     unchecked and counted in `Result.UncheckedImportCalls`. That is
	//     the M37 behaviour, kept so an already-recorded trace still
	//     replays rather than being rejected for its age.
	//
	// It is derived from the records, not declared: any crossing recovered
	// from an import-labelled marker pair sets it. A producer cannot claim
	// the newer format without having used it.
	MarkersIdentifyImports bool
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
// Loading a boundary log
// ---------------------------------------------------------------------------

// The records of a boundary log, as `ctbl.go` decodes them. Each is one
// record of the recording itself; `traceEvent` carries exactly one of them.
type traceEvent struct {
	Path         *string
	Step         *stepRecord
	Function     *functionRecord
	Call         *callRecord
	Return       *returnRecord
	Value        *valueRecord
	VariableName *string
	Event        *eventRecord
}

type stepRecord struct {
	PathID uint32
	Line   int64
}

type functionRecord struct {
	Name   string
	PathID uint32
	Line   int64
}

type callRecord struct {
	FunctionID uint32
	// Args is always empty on the browser path — `browser_session.js`
	// sends `{kind:"Call", fnId, args: []}` and delivers the arguments as
	// separate `Value` records. Decoded anyway so a producer that starts
	// populating it does not silently lose data.
	Args []argRecord
}

type argRecord struct {
	VariableID uint32
	Value      rawValue
}

type returnRecord struct {
	ReturnValue rawValue
}

type valueRecord struct {
	VariableID uint32
	Value      rawValue
}

// LoadRecording reads the boundary recording stored in a `.ct`: the CTFS
// container `record-web` writes, whose `boundary.log` internal file is the
// boundary log (`Browser-Recording-Container.md` §2).
//
// A directory, or a `trace.json`, is the retired three-file JSON layout and
// is refused by name rather than probed.
func LoadRecording(path string) (*Recording, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("opening boundary recording %s: %w", path, err)
	}
	if st.IsDir() || filepath.Base(path) == "trace.json" {
		return nil, fmt.Errorf(
			"boundary recording %s is the retired three-file JSON layout "+
				"(trace.json); a boundary recording is the single-file `<program>.ct` "+
				"`record-web` writes, with its boundary log stored inside it", path)
	}
	c, err := ctfs.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening boundary recording %s: %w", path, err)
	}
	data, err := c.ReadFile(BoundaryLogFileName)
	if err != nil {
		return nil, fmt.Errorf(
			"boundary recording %s carries no %s: it is a CTFS container, but not "+
				"one `record-web` wrote (%w)", path, BoundaryLogFileName, err)
	}
	return DecodeRecording(data, path)
}

// DecodeRecording reads a complete boundary log — the bytes of a
// `boundary.log` — into a Recording. `source` names it in diagnostics.
//
// The log must end with its `End` frame: a finished recording that stops
// short is damaged, not merely early.
func DecodeRecording(data []byte, source string) (*Recording, error) {
	d := &frameDecoder{}
	d.feed(data)
	var events []traceEvent
	for {
		ev, ok, err := d.next()
		if err != nil {
			return nil, fmt.Errorf("decoding the boundary log of %s: %w", source, err)
		}
		if !ok {
			break
		}
		events = append(events, *ev)
	}
	if d.pending() > 0 || !d.ended {
		return nil, fmt.Errorf(
			"the boundary log of %s is incomplete: it ends without its End frame "+
				"(%d trailing byte(s)); the recording was not finalised", source, d.pending())
	}

	rec := recordingFromHeader(d.header, source)
	for i := range events {
		if events[i].Path != nil {
			rec.Paths = append(rec.Paths, *events[i].Path)
		}
	}
	crossings, marked, streamed, err := reconstructCrossings(events)
	if err != nil {
		return nil, fmt.Errorf("recovering boundary crossings from %s: %w", source, err)
	}
	rec.Crossings = crossings
	rec.MarkersIdentifyImports = marked
	rec.HostState = streamed
	return rec, nil
}

// recordingFromHeader is a Recording carrying a header's metadata and no
// crossings yet.
func recordingFromHeader(h *logHeader, source string) *Recording {
	rec := &Recording{Source: source}
	if h != nil {
		rec.Program = h.Program
		rec.Args = h.Args
		rec.Workdir = h.Workdir
		rec.Recorder = h.RecorderName
	}
	return rec
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
//     `FUNC_KIND_EXPORT`. It DOES emit a pair of correlation markers, as
//     an export does: `replace_imported_call` in the instrumenter pushes
//     `push_realm_boundary(..., FUNC_KIND_IMPORT, ...)` on both sides of
//     the edge. As of M39 those markers name the edge — `show_value` is
//     `wasm import #<n>` for an import and `wasm export #<n>` for an
//     export — so an import crossing IS bracketed on disk, by its markers,
//     even when it carries no values at all.
//
// The reconstruction below therefore brackets exports on `Call`/`Return`,
// brackets imports on their `wasm import #<n>` marker pair, and fills the
// argument/result tuples in from the value runs that fall inside it.
//
// # Recordings older than M39
//
// A recording produced before M39 spells BOTH edges `wasm export #<n>`, so
// its import markers cannot be attributed to an import by their own
// content. Such a recording is still accepted, and its imports are still
// recovered — from their value runs, which is what M37 did and what the
// `role == "ret"` and `closeDanglingImports` paths below implement. Only
// one shape is lost with it: an import whose signature is `() -> ()`
// contributes no value runs either, so no crossing is recovered for it and
// replay counts the call in `Result.UncheckedImportCalls` rather than
// checking it. `Recording.MarkersIdentifyImports` is what tells the two
// vintages apart, and it is derived from the records rather than declared.
//
// # One ambiguity that survives in both vintages
//
// Two adjacent import crossings of the same import, the first with
// arguments but no results, are indistinguishable *by their value runs*
// from one crossing whose argument run was recorded twice.
// `reconstructCrossings` closes the open crossing when a second argument
// run for the same import arrives, which is the reading that keeps call
// counts right. With M39 markers the question does not arise: each
// crossing has its own `ENTER`/`LEAVE` pair.

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

// reconstructCrossings recovers every crossing from a whole boundary log,
// reports whether the recording's realm markers name the import edge (see
// `Recording.MarkersIdentifyImports`), and returns the spec §3.3 / §3.4
// state the event stream carried, or nil if it carried none (M44b).
func reconstructCrossings(events []traceEvent) ([]Crossing, bool, *HostState, error) {
	a := newAssembler()
	for i := range events {
		if err := a.push(&events[i]); err != nil {
			return nil, false, nil, err
		}
	}
	if err := a.finish(); err != nil {
		return nil, false, nil, err
	}
	return a.crossings, a.sawImportMarker, a.hostState, nil
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

// importLabel renders the binding label of one import index, so the
// assembler and `parseImportLabel` cannot disagree about its spelling.
func importLabel(index uint32) string {
	return importLabelPrefix + strconv.FormatUint(uint64(index), 10)
}

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

// ---------------------------------------------------------------------------
// Realm markers
// ---------------------------------------------------------------------------
//
// A realm marker is what `browser_session.js`'s `emitRealmBoundary`
// produces on each side of a crossing, rendered by the backend-manager's
// `JsonFileCtfsWriter` into a `RecordEvent`:
//
//	Event{kind: 12,
//	      metadata: `{…"boundary_id":"js-wasm-realm","direction":"recv",
//	                  "show_value":"wasm import #3"…}`,
//	      content:  `{"key":"7","payload":"wasm import #3"}`}
//
// `metadata` is a `MarkerPayload` document, the correlation-marker encoding
// every CodeTracer recording uses for this record, not something this package
// chose.
//
// The pair is primarily a cross-recording correlation key (the page's own
// JS recording carries the mirrored half), and the db-backend pairs them by
// `key`. What this package takes from them is narrower and purely local:
// `show_value` names which edge the marker belongs to and which index, so a
// crossing that leaves nothing else on disk can still be bracketed.

// eventKindTraceLogEvent is `EVENT_KIND_TRACE_LOG_EVENT`, the `kind` the
// daemon gives a correlation marker.
const eventKindTraceLogEvent = 12

// jsWasmRealmBoundary is `browser_session.js`'s `JS_WASM_REALM_BOUNDARY`.
// Markers of any other boundary belong to some other producer on the page
// and say nothing about this module's crossings.
const jsWasmRealmBoundary = "js-wasm-realm"

// The two `show_value` templates `emitRealmBoundary` renders. They differ
// only as of M39; before it, an import edge also said "wasm export #".
const (
	exportMarkerPrefix = "wasm export #"
	importMarkerPrefix = "wasm import #"
)

// markerEnterDirection is the `direction` an ENTER marker carries.
// `emitRealmBoundary` renders `REALM_DIRECTION_LEAVE` as "send" (the value
// flows WASM -> host) and `REALM_DIRECTION_ENTER` as "recv".
const (
	markerEnterDirection = "recv"
	markerLeaveDirection = "send"
)

// eventRecord is one `Event` record of the boundary log.
type eventRecord struct {
	Kind     int
	Metadata string
	Content  string
}

// markerMetadata is the subset of the nested `metadata` string this package
// reads. `show_value` may be JSON `null`, which leaves the field empty.
type markerMetadata struct {
	BoundaryID string `json:"boundary_id"`
	Direction  string `json:"direction"`
	ShowValue  string `json:"show_value"`
}

// realmMarker is one decoded `js-wasm-realm` marker.
type realmMarker struct {
	// kind is the edge the marker names.
	kind CrossingKind
	// index is the import index (for an import edge) or the export index
	// (for an export edge, where this package does not use it — exports are
	// identified by name, and the two numberings are unrelated).
	index uint32
	// enter is true for the marker at the call site, false at the return
	// site.
	enter bool
}

// parseRealmMarker decodes one `Event` record as a `js-wasm-realm` marker.
//
// It returns ok=false for anything else — a marker of another boundary, an
// event of another kind, a `show_value` that is not one of the two
// templates, or malformed JSON. That is deliberately quiet rather than
// fatal: `Event` records are an open extension point on this path (the JS
// recorder puts HTTP and other domain markers into its own recordings), so
// an unrecognised one must not fail a recording. Nothing is lost by
// ignoring one, because a marker this package cannot read contributes no
// crossing — it only delimits value runs, which the caller does anyway.
func parseRealmMarker(ev *eventRecord) (realmMarker, bool) {
	if ev.Kind != eventKindTraceLogEvent {
		return realmMarker{}, false
	}
	var meta markerMetadata
	if err := json.Unmarshal([]byte(ev.Metadata), &meta); err != nil {
		return realmMarker{}, false
	}
	if meta.BoundaryID != jsWasmRealmBoundary {
		return realmMarker{}, false
	}

	var m realmMarker
	switch {
	case strings.HasPrefix(meta.ShowValue, importMarkerPrefix):
		m.kind = CrossingImport
		m.index = 0
		if n, err := strconv.ParseUint(
			meta.ShowValue[len(importMarkerPrefix):], 10, 32); err == nil {
			m.index = uint32(n)
		} else {
			return realmMarker{}, false
		}
	case strings.HasPrefix(meta.ShowValue, exportMarkerPrefix):
		m.kind = CrossingExport
		if n, err := strconv.ParseUint(
			meta.ShowValue[len(exportMarkerPrefix):], 10, 32); err == nil {
			m.index = uint32(n)
		} else {
			return realmMarker{}, false
		}
	default:
		return realmMarker{}, false
	}

	switch meta.Direction {
	case markerEnterDirection:
		m.enter = true
	case markerLeaveDirection:
		m.enter = false
	default:
		return realmMarker{}, false
	}
	return m, true
}
