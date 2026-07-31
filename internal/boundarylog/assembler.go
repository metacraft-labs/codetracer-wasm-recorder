package boundarylog

import (
	"encoding/json"
	"fmt"
	"strings"
)

// assembler recovers boundary crossings from the browser's rendered trace
// records, one record at a time.
//
// It is the incremental form of what `recording.go` documents at length under
// "How a crossing appears in a browser `.ct`" — **read that comment before
// touching this file**; every rule it states is implemented here and nowhere
// else. `reconstructCrossings` is a thin loop over `push`, so the batch path
// and the streaming path of `WASM-Replay-Snapshots-And-Slices.md` §2 share one
// implementation and cannot drift: everything that pins the format
// (`browser_format_test.go`, `recording_test.go`, the committed browser
// recording) pins the streaming reader too.
//
// # Call groups
//
// The streaming pipeline needs a unit it can hand to the replay driver, and the
// assembler's is a **call group**: one top-level export crossing together with
// every crossing nested inside it, complete. A group closes when the crossing
// stack empties.
//
// That is the right granularity rather than a compromise. A quiescent point is
// by definition a moment when no exported function is executing (§3), so the
// gap between two call groups is exactly a quiescent point, and a snapshot
// cannot be taken anywhere else. Yielding a finer unit would buy nothing: the
// replay could not act on half an exported call, and no snapshot could be
// emitted inside one.
type assembler struct {
	// functions and varNames are the producer's id tables, appended to as
	// `Function` and `VariableName` records arrive.
	functions []functionRecord
	varNames  []string

	// crossings holds every crossing recovered so far, in call order.
	crossings []Crossing
	// stack holds crossings whose start has been seen but whose end has not.
	stack []openCrossing
	// pending is the value run currently accumulating.
	pending *valueRun

	// groupStart is the index in `crossings` at which the group currently
	// being assembled begins.
	groupStart int
	// groupEnds holds the exclusive end index of each completed call group, so
	// group i spans crossings[start_i:groupEnds[i]].
	groupEnds []int

	// sawImportMarker records that at least one `wasm import #<n>` realm
	// marker arrived, i.e. that this recording's producer spells the two
	// edges apart (M39). See `Recording.MarkersIdentifyImports`.
	sawImportMarker bool

	// hostState accumulates the spec §3.3 / §3.4 records the stream
	// carries (M44b), or stays nil for a recording that carries none.
	//
	// It lives here, in the shared reconstruction, for the same reason
	// the crossings do: `reconstructCrossings` and `StreamReader` are two
	// loops over one `assembler`, so the batch and streaming drivers
	// cannot disagree about what a recording says without the disagreement
	// being visible in this one place.
	hostState *HostState
}

func newAssembler() *assembler { return &assembler{} }

// closeDanglingImports pops open import crossings from the top of the stack.
//
// An import recovered from its *value runs alone* — every import in a
// recording older than M39 — is bracketed by those runs, so anything other
// than its result run arriving means it returned nothing and is over. `keep`
// is the label of a run that legitimately continues the crossing on top (its
// result run); pass "" to close everything.
//
// A crossing bracketed by its own realm markers is exempt unless `force`:
// it is closed by its `LEAVE` marker and by nothing else, which is what
// makes a `() -> ()` crossing — with no runs at all to be delimited by —
// recoverable. `force` is for end of input, where an unclosed crossing is a
// truncation rather than a structure to be respected.
func (a *assembler) closeDanglingImports(keep string, force bool) {
	for len(a.stack) > 0 {
		top := a.stack[len(a.stack)-1]
		c := &a.crossings[top.idx]
		if c.Kind != CrossingImport {
			return
		}
		if top.label == keep {
			return
		}
		if c.markerBracketed && !force {
			return
		}
		a.stack = a.stack[:len(a.stack)-1]
	}
}

// closeRun applies the accumulated value run to the crossing stack.
func (a *assembler) closeRun() error {
	if a.pending == nil {
		return nil
	}
	run := *a.pending
	a.pending = nil

	importIdx, isImport := parseImportLabel(run.label)

	// Close any import crossing this run does not continue. A run that is not
	// the open import's own result run means that import is over — including a
	// fresh argument run for the SAME import, which is a second call rather
	// than a continuation.
	keep := ""
	if run.role == "ret" {
		keep = run.label
	}
	a.closeDanglingImports(keep, false)

	if run.role == "arg" {
		if isImport {
			// An argument run opens an import crossing.
			a.crossings = append(a.crossings, Crossing{
				Seq:   len(a.crossings),
				Depth: len(a.stack),
				Kind:  CrossingImport,
				Index: importIdx,
				Args:  run.values,
			})
			a.stack = append(a.stack, openCrossing{idx: len(a.crossings) - 1, label: run.label})
			return nil
		}
		// An export's argument run belongs to the frame `Call` just opened.
		if len(a.stack) == 0 || a.stack[len(a.stack)-1].label != run.label {
			return fmt.Errorf(
				"argument run for %q has no matching open export frame; the "+
					"recording's Call/Return bracketing and its value bindings "+
					"disagree", run.label)
		}
		a.crossings[a.stack[len(a.stack)-1].idx].Args = run.values
		return nil
	}

	// role == "ret"
	if len(a.stack) > 0 && a.stack[len(a.stack)-1].label == run.label {
		top := a.stack[len(a.stack)-1]
		a.crossings[top.idx].Results = run.values
		a.crossings[top.idx].hasResults = true
		if isImport && !a.crossings[top.idx].markerBracketed {
			// A run-bracketed import ends with its result run; exports are
			// popped by their `Return` record, and a marker-bracketed import
			// by its `LEAVE` marker (which the producer emits AFTER this
			// run — see the framing contract in `hooks.rs`).
			a.stack = a.stack[:len(a.stack)-1]
		}
		return nil
	}
	if isImport {
		// A result run with no open crossing means an import that takes no
		// arguments: its only on-disk trace is this run.
		a.crossings = append(a.crossings, Crossing{
			Seq:        len(a.crossings),
			Depth:      len(a.stack),
			Kind:       CrossingImport,
			Index:      importIdx,
			Results:    run.values,
			hasResults: true,
		})
		return nil
	}
	return fmt.Errorf(
		"result run for %q has no matching open export frame; the recording's "+
			"Call/Return bracketing and its value bindings disagree", run.label)
}

// push feeds one decoded trace record.
func (a *assembler) push(ev *traceEvent) error {
	switch {
	case ev.Function != nil:
		if err := a.closeRun(); err != nil {
			return err
		}
		a.functions = append(a.functions, *ev.Function)

	case ev.VariableName != nil:
		a.varNames = append(a.varNames, *ev.VariableName)

	case ev.Value != nil:
		if err := a.pushValue(ev); err != nil {
			return err
		}

	case ev.Call != nil:
		if err := a.closeRun(); err != nil {
			return err
		}
		id := int(ev.Call.FunctionID)
		if id < 0 || id >= len(a.functions) {
			return fmt.Errorf(
				"Call record references function_id %d but only %d Function "+
					"records have been seen", id, len(a.functions))
		}
		name := a.functions[id].Name
		a.crossings = append(a.crossings, Crossing{
			Seq:   len(a.crossings),
			Depth: len(a.stack),
			Kind:  CrossingExport,
			Name:  name,
		})
		a.stack = append(a.stack, openCrossing{idx: len(a.crossings) - 1, label: name})

	case ev.Event != nil:
		if err := a.pushEvent(ev); err != nil {
			return err
		}

	case ev.Return != nil:
		if err := a.closeRun(); err != nil {
			return err
		}
		// An import still open when its caller returns had no results; the
		// export's `Return` is proof it is over. A marker-bracketed import is
		// NOT closed here: its `LEAVE` marker precedes the export's `Return`
		// in a well-formed recording, so one still open means the recording
		// is unbalanced, which the check below reports.
		a.closeDanglingImports("", false)
		if len(a.stack) == 0 {
			return fmt.Errorf("Return record with no open export frame")
		}
		top := a.stack[len(a.stack)-1]
		if a.crossings[top.idx].Kind != CrossingExport {
			return fmt.Errorf(
				"Return record closes %s, but an export frame was expected; "+
					"the recording is structurally unbalanced",
				a.crossings[top.idx].Describe())
		}
		a.stack = a.stack[:len(a.stack)-1]

	default:
		// Path / Step records carry no boundary structure of their own, but
		// they DO delimit value runs: `flushValues` emits exactly one `Step`
		// immediately before each run, so a `Step` closes whatever run
		// preceded it.
		if ev.Path != nil || ev.Step != nil {
			if err := a.closeRun(); err != nil {
				return err
			}
		}
	}

	a.noteGroupBoundary()
	return nil
}

// pushEvent handles one `Event` record.
//
// Every `Event` closes the value run in flight, exactly as a `Step` does:
// the producer flushes an import's argument tuple *inside* the realm-marker
// hook, immediately before emitting the marker, so the run is complete by
// the time the marker record lands.
//
// Beyond that, only a `js-wasm-realm` marker naming an IMPORT edge carries
// boundary structure — and it carries all of it for a crossing whose
// signature is `() -> ()`, which leaves nothing else on disk at all.
// Everything else (an export's own markers, a domain marker some other
// producer on the page emitted) is a delimiter and nothing more.
func (a *assembler) pushEvent(ev *traceEvent) error {
	if err := a.closeRun(); err != nil {
		return err
	}
	// A host-state record (M44b) is not boundary structure: it describes
	// the state a crossing starts from or what the host did during one.
	// It is folded into the accumulating `HostState` and contributes no
	// crossing, so it must be recognised before the realm-marker parse
	// rather than after — `parseRealmMarker` would return ok=false for it
	// and the record would be silently dropped.
	if hs, ok, err := parseHostStateMarker(*ev.Event); err != nil {
		return err
	} else if ok {
		folded, err := foldHostStateMarker(a.hostState, hs)
		if err != nil {
			return err
		}
		a.hostState = folded
		return nil
	}
	m, ok := parseRealmMarker(*ev.Event)
	if !ok || m.kind != CrossingImport {
		return nil
	}
	// Reached only by a producer that spells the two edges apart (M39).
	a.sawImportMarker = true
	if m.enter {
		a.openMarkedImport(m.index)
		return nil
	}
	return a.closeMarkedImport(m.index)
}

// openMarkedImport begins the crossing an `ENTER` marker names.
//
// The argument run, if the import has one, was flushed one record earlier
// and has already opened this very crossing (`closeRun`'s "arg" arm). That
// is not a race to be resolved but the same crossing seen twice, so the
// marker adopts it rather than opening a second — and either way the
// crossing lands at this position in the append order, which is what
// `browser_session.js` predicts when it anchors a spec §3.4 mutation.
func (a *assembler) openMarkedImport(index uint32) {
	if n := len(a.stack); n > 0 {
		top := a.stack[n-1]
		c := &a.crossings[top.idx]
		if c.Kind == CrossingImport && c.Index == index && !c.markerBracketed {
			c.markerBracketed = true
			return
		}
	}
	a.crossings = append(a.crossings, Crossing{
		Seq:             len(a.crossings),
		Depth:           len(a.stack),
		Kind:            CrossingImport,
		Index:           index,
		markerBracketed: true,
	})
	a.stack = append(a.stack, openCrossing{
		idx: len(a.crossings) - 1, label: importLabel(index),
	})
}

// closeMarkedImport ends the crossing a `LEAVE` marker names.
//
// A mismatch is refused rather than repaired. The two markers are emitted
// by the same splice around one call instruction, so they cannot legitimately
// disagree; a recording where they do is one whose crossing structure this
// replayer would have to guess at, and guessing is what spec §8 forbids.
func (a *assembler) closeMarkedImport(index uint32) error {
	if n := len(a.stack); n > 0 {
		top := a.stack[n-1]
		c := &a.crossings[top.idx]
		if c.Kind == CrossingImport && c.Index == index && c.markerBracketed {
			a.stack = a.stack[:n-1]
			return nil
		}
	}
	open := "nothing"
	if n := len(a.stack); n > 0 {
		open = a.crossings[a.stack[n-1].idx].Describe()
	}
	return fmt.Errorf(
		"a realm marker closes import #%d, but the innermost open crossing is "+
			"%s; the recording's realm markers are not properly nested, so its "+
			"crossing structure cannot be recovered (spec §8)", index, open)
}

// pushValue handles one `Value` record.
func (a *assembler) pushValue(ev *traceEvent) error {
	id := int(ev.Value.VariableID)
	if id < 0 || id >= len(a.varNames) {
		return fmt.Errorf(
			"Value record references variable_id %d but only %d "+
				"VariableName records have been seen", id, len(a.varNames))
	}
	name := a.varNames[id]
	label, role, slot, ok := parseBindingName(name)
	if !ok {
		// A binding the boundary model does not produce. Close any run in
		// flight and ignore it: a future producer may legitimately add
		// non-boundary bindings, and dropping a value we do not understand is
		// safe here precisely because replay independently re-derives
		// everything inside the module.
		return a.closeRun()
	}
	var odv onDiskValue
	if err := json.Unmarshal(ev.Value.Value, &odv); err != nil {
		return fmt.Errorf("decoding value for binding %q: %w", name, err)
	}
	// A run ends when the binding's (label, role) changes, or when the slot
	// index restarts at 0 — the latter is what distinguishes two back-to-back
	// calls of the SAME import from one over-long tuple. (`Step` closes runs
	// too, and the producer emits one before every flush, so this is a second,
	// independent signal rather than the only one.)
	if a.pending != nil && (a.pending.label != label || a.pending.role != role ||
		(slot == 0 && len(a.pending.values) > 0)) {
		if err := a.closeRun(); err != nil {
			return err
		}
	}
	if a.pending == nil {
		a.pending = &valueRun{label: label, role: role}
	}
	if slot != len(a.pending.values) {
		return fmt.Errorf(
			"binding %q arrived at slot %d but %d value(s) of this tuple "+
				"have been seen; the recording's value runs are not in slot order",
			name, slot, len(a.pending.values))
	}
	a.pending.values = append(a.pending.values, odv.raw())
	return nil
}

// noteGroupBoundary records a completed call group when the crossing stack has
// emptied and no value run is in flight.
func (a *assembler) noteGroupBoundary() {
	if len(a.stack) != 0 || a.pending != nil {
		return
	}
	if len(a.crossings) <= a.groupStart {
		return
	}
	a.groupEnds = append(a.groupEnds, len(a.crossings))
	a.groupStart = len(a.crossings)
}

// finish closes the assembler at end of input.
//
// An unbalanced stream is refused, exactly as the batch path always has:
// `codetracer-wasm-instrumenter`'s own reference decoder rejects it too — an
// export that exits by branching to its own function label emits no
// `__ct_emit_return` and leaves the crossing open (see M35b). Replaying such a
// recording would silently mis-attribute everything after the hole (spec §8).
func (a *assembler) finish() error {
	if err := a.closeRun(); err != nil {
		return err
	}
	// `force`: at end of input an import whose `LEAVE` marker never arrived
	// is a truncated recording, not a structure to preserve. Closing it here
	// keeps the classification in `StreamReader.finishStream` on the export
	// that encloses it, which is the crossing that actually cannot be
	// replayed.
	a.closeDanglingImports("", true)

	if len(a.stack) > 0 {
		open := make([]string, 0, len(a.stack))
		for _, s := range a.stack {
			open = append(open, a.crossings[s.idx].Describe())
		}
		return fmt.Errorf(
			"boundary recording is structurally unbalanced: %d crossing(s) never "+
				"closed (%s). An exported function that exits by branching to its "+
				"own label emits no return hook; such a recording cannot be replayed "+
				"faithfully (spec §8)",
			len(a.stack), strings.Join(open, ", "))
	}
	a.noteGroupBoundary()
	return nil
}

// groups reports how many complete call groups have been assembled.
func (a *assembler) groups() int { return len(a.groupEnds) }

// group returns the crossings of the i-th completed call group.
func (a *assembler) group(i int) []Crossing {
	start := 0
	if i > 0 {
		start = a.groupEnds[i-1]
	}
	return a.crossings[start:a.groupEnds[i]]
}

// openCrossings reports how many crossings are mid-flight, which is how a
// streaming reader tells "the producer stopped between calls" from "the
// producer stopped in the middle of one".
func (a *assembler) openCrossings() int { return len(a.stack) }
