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
}

func newAssembler() *assembler { return &assembler{} }

// closeDanglingImports pops open import crossings from the top of the stack.
//
// An import is bracketed by its own value runs, so anything other than its
// result run arriving means it returned nothing and is over. `keep` is the
// label of a run that legitimately continues the crossing on top (its result
// run); pass "" to close everything.
func (a *assembler) closeDanglingImports(keep string) {
	for len(a.stack) > 0 {
		top := a.stack[len(a.stack)-1]
		if a.crossings[top.idx].Kind != CrossingImport {
			return
		}
		if top.label == keep {
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
	a.closeDanglingImports(keep)

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
		if isImport {
			// Imports are bracketed by their runs; exports are popped by their
			// `Return` record.
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

	case ev.Return != nil:
		if err := a.closeRun(); err != nil {
			return err
		}
		// An import still open when its caller returns had no results; the
		// export's `Return` is proof it is over.
		a.closeDanglingImports("")
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
		// Path / Step / Event records carry no boundary structure of their own,
		// but they DO delimit value runs: `flushValues` emits exactly one
		// `Step` immediately before each run, so a `Step` closes whatever run
		// preceded it.
		if ev.Path != nil || ev.Event != nil || ev.Step != nil {
			if err := a.closeRun(); err != nil {
				return err
			}
		}
	}

	a.noteGroupBoundary()
	return nil
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
	a.closeDanglingImports("")

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
