package boundarylog

// The streaming half of `WASM-Replay-Snapshots-And-Slices.md` §2: a replaying
// recorder that consumes the browser's boundary stream *as it arrives* and
// re-executes in lockstep, so that "when the page stops, the snapshots are
// already there".
//
// `Replay` is pull-based over a fully parsed recording: it walks
// `TopLevelExports()` of a `Recording` that is complete before the first call is
// driven. `StreamingReplay` drives the same machinery — the same import stubs,
// the same divergence checks, the same quiescent-point hook — off a
// `StreamReader` instead, one call group at a time.
//
// # Why the two are not one function
//
// They differ in what they know, not in what they do. The batch driver knows
// how many exported calls there are, so it can resolve a `[from,to)` range,
// seed a crossing cursor for a seek and check at the end that every recorded
// crossing was consumed. The streaming driver knows none of that until the
// stream ends, and cannot seek at all: there is nothing to seek *into* while
// the recording is still being made. Everything they share — instantiation,
// host modules, §3.3 initial state, the per-crossing checks and the export
// invocation — is shared code, in `replayer`.

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/tetratelabs/wazero/api"
)

// StreamResult reports what a streaming replay did.
type StreamResult struct {
	Result
	// Truncation is non-nil when the producer stopped before finishing the
	// recording. It is **not** an error return: every call group replayed
	// before the truncation was driven from complete crossings, so the trace
	// and the snapshots produced for them are faithful. The caller decides
	// whether a prefix is worth keeping; this reports what it has.
	Truncation *TruncationError
}

// StreamingReplay re-executes a module against a boundary recording that is
// still being produced.
//
// It drives one exported call per group the reader yields, invoking
// `Options.AtQuiescentPoint` at point 0 before the first call and after every
// call returns — which is what makes snapshots and slices appear *during* the
// recording rather than in a pass at the end. Nothing here waits for the stream
// to finish.
//
// `opts.Recording` supplies the recording's metadata and its optional §3.3 /
// §3.4 host state; its `Crossings` must be empty, and are appended to as the
// stream is consumed, so that at any moment it holds exactly the crossings
// replayed so far.
func StreamingReplay(ctx context.Context, opts Options, src *StreamReader) (StreamResult, error) {
	var out StreamResult
	if src == nil {
		return out, fmt.Errorf("boundary streaming replay: no stream reader supplied")
	}
	rec := opts.Recording
	if rec == nil {
		return out, fmt.Errorf("boundary streaming replay: no recording supplied")
	}
	if len(rec.Crossings) != 0 {
		return out, fmt.Errorf(
			"boundary streaming replay: the recording already carries %d crossing(s); "+
				"a streaming replay appends the crossings it consumes, so it must be "+
				"given the recording's metadata only", len(rec.Crossings))
	}
	if opts.FromPoint != 0 || opts.ToPoint != 0 || opts.Resume != nil {
		// Seeking is a property of a recording that exists. While one is being
		// produced there is nothing to seek into, and accepting the fields
		// silently would let a caller believe a range was honoured when the
		// whole stream was replayed.
		return out, fmt.Errorf(
			"boundary streaming replay: FromPoint / ToPoint / Resume are not " +
				"meaningful while a recording is still being produced — the range they " +
				"would name does not exist yet. Materialise a sub-range from the " +
				"finished container instead")
	}

	// The streaming driver cannot decide, at the moment of the call, whether an
	// unmatched `() -> ()` import call is a §6 divergence or an M37 unchecked
	// call: `Recording.MarkersIdentifyImports` is a property of the whole
	// recording and the whole recording has not arrived. It therefore defers
	// the question and `resolveDeferredValuelessImports` answers it below,
	// against a witness that is final. See `serviceUnwitnessedValuelessImport`.
	r := &replayer{
		opts: opts, providers: map[string]api.Module{},
		deferValuelessImports: true,
	}
	guest, err := r.prepare(ctx)
	if err != nil {
		return out, err
	}

	// Quiescent point 0: the freshly instantiated module, before any recorded
	// call. This is where a slice opens and where the first snapshot is taken.
	point := 0
	if err := r.atQuiescentPoint(point, guest); err != nil {
		return out, err
	}

	for {
		group, err := src.NextGroup()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if t, ok := IsTruncation(err); ok {
				// A truncated stream ends the replay without invalidating it.
				// Every group already driven came from complete crossings.
				out.Truncation = t
				break
			}
			return out, err
		}

		idx, err := r.appendGroup(rec, group)
		if err != nil {
			return out, err
		}
		c := &rec.Crossings[idx]
		if err := r.checkExportCrossing(c); err != nil {
			return out, err
		}
		if err := r.callExport(ctx, guest, c); err != nil {
			return out, err
		}
		point++
		if err := r.atQuiescentPoint(point, guest); err != nil {
			return out, err
		}
	}

	out.Result = r.result
	out.Result.FromPoint, out.Result.ToPoint = 0, point

	// The stream is over, so `rec.MarkersIdentifyImports` now says exactly what
	// `LoadRecording` would have said about the same bytes. Any value-less
	// import call whose classification was deferred is decided here, before the
	// checks below, because the batch driver would have reported it at the call
	// itself — which precedes every one of them.
	if err := r.resolveDeferredValuelessImports(rec, out.Truncation); err != nil {
		return out, err
	}

	// Every crossing the stream delivered must have been consumed. A leftover
	// means the module stopped calling out earlier than it did when recorded —
	// the same check `Replay` makes at the end of a whole recording, and just
	// as meaningful here because the stream is replayed from its start.
	if out.Truncation == nil && r.cursor != len(rec.Crossings) {
		next := &rec.Crossings[r.cursor]
		return out, &DivergenceError{
			What: "crossing count",
			Where: fmt.Sprintf("end of stream (%d of %d crossings consumed)",
				r.cursor, len(rec.Crossings)),
			Recorded: next.Describe(),
			Actual:   "the stream ended without the replay reaching it",
			Detail: "the module stopped crossing the boundary earlier than it did " +
				"when recorded",
		}
	}
	return out, nil
}

// resolveDeferredValuelessImports answers, at end of stream, the question
// `serviceUnwitnessedValuelessImport` refused to answer at the call.
//
// This is M51's fix. The three outcomes are exhaustive and none of them is a
// quiet fallback:
//
//   - the witness ended TRUE — the recording's markers do name the import
//     edge, so the batch driver would have been strict from its first call and
//     would have diverged where this replay deferred. The stashed divergence is
//     returned, and the two drivers reach the same verdict.
//   - the witness ended FALSE on a complete stream — the recording carries no
//     import-labelled marker anywhere, which is exactly what `LoadRecording`
//     would have derived from the same bytes. The batch driver would have taken
//     the same M37 arm at the same call, so the two runs are identical and the
//     count in `Result.UncheckedImportCalls` is the shared answer.
//   - the witness ended FALSE on a TRUNCATED stream — "false" here means "no
//     import marker arrived before the producer stopped", which is not the same
//     statement and cannot be promoted to it. The classification is genuinely
//     unresolvable, so it is refused. Accepting it would be a guess, and a
//     guess in the permissive direction is precisely the degradation §8 forbids.
func (r *replayer) resolveDeferredValuelessImports(rec *Recording, trunc *TruncationError) error {
	if r.deferredValueless == nil {
		return nil
	}
	if rec.MarkersIdentifyImports {
		return r.deferredValueless
	}
	if trunc != nil {
		return fmt.Errorf(
			"a call to a `() -> ()` import could not be classified: %s. Whether such "+
				"a call is a spec §6 divergence or an unchecked call (the pre-M39 "+
				"behaviour) depends on whether the recording's realm markers name the "+
				"import edge, and no such marker had arrived when the stream was cut "+
				"short (%s). A prefix cannot settle a property of the whole recording, "+
				"so the replay is refused rather than accepted under a guess (spec §8). "+
				"Replay the finished recording instead. What the strict reading would "+
				"have reported: %w",
			r.deferredValuelessSite(), trunc.Kind, r.deferredValueless)
	}
	return nil
}

// deferredValuelessSite names where the deferred classification arose, for the
// truncation diagnostic above.
func (r *replayer) deferredValuelessSite() string {
	return fmt.Sprintf("after %d exported and %d imported call(s)",
		r.result.ExportCalls, r.result.ImportCalls)
}

// appendGroup adds one call group's crossings to the growing recording and
// returns the index of the group's top-level export crossing.
//
// A group is exactly one such crossing plus whatever it nested, because that is
// how `assembler` delimits groups — the stack emptying. The checks below are
// therefore assertions about the assembler's contract as much as about the
// recording, and each of them, if it fired, would mean a call was about to be
// driven against the wrong crossing.
func (r *replayer) appendGroup(rec *Recording, group []Crossing) (int, error) {
	if len(group) == 0 {
		return 0, fmt.Errorf("boundary streaming replay: an empty call group arrived")
	}
	base := len(rec.Crossings)
	top := -1
	var nested []*Crossing
	for i := range group {
		c := group[i]
		// The recording's format witness, accumulated as it arrives: a
		// crossing bracketed by import-labelled realm markers can only come
		// from an M39 producer, so once true it stays true.
		//
		// It is **monotone, not final**. A true value here is sound — the
		// marker really did arrive — but a false one only means "not yet",
		// and `LoadRecording` derives the same flag from the WHOLE document.
		// That gap is M51: a `() -> ()` import call made while this is still
		// false must not be classified on it, and is not. See
		// `serviceUnwitnessedValuelessImport` and
		// `resolveDeferredValuelessImports`, which decide once the stream has
		// ended and the two derivations agree by construction.
		if c.markerBracketed {
			rec.MarkersIdentifyImports = true
		}
		if c.Seq != base+i {
			return 0, fmt.Errorf(
				"boundary streaming replay: crossing #%d arrived at position %d of the "+
					"stream; the crossing numbering and the replay order disagree",
				c.Seq, base+i)
		}
		if c.Kind == CrossingExport {
			if c.Depth > 0 {
				rec.Crossings = append(rec.Crossings, c)
				nested = append(nested, &rec.Crossings[len(rec.Crossings)-1])
				continue
			}
			if top >= 0 {
				return 0, fmt.Errorf(
					"boundary streaming replay: a call group carried two top-level "+
						"exported calls (%s and %s); groups are delimited by the crossing "+
						"stack emptying, so this cannot happen unless the assembler is "+
						"wrong", rec.Crossings[top].Describe(), c.Describe())
			}
			top = len(rec.Crossings)
		}
		rec.Crossings = append(rec.Crossings, c)
	}
	if err := refuseNestedExports(nested); err != nil {
		return 0, err
	}
	if top < 0 {
		return 0, fmt.Errorf(
			"boundary streaming replay: a call group of %d crossing(s) carried no "+
				"top-level exported call. The browser records import crossings only "+
				"while an exported function is executing, so a group without one means "+
				"the recording's structure is not the one this replayer can drive",
			len(group))
	}
	if top != base {
		return 0, fmt.Errorf(
			"boundary streaming replay: a call group's top-level exported call is its "+
				"%dth crossing, not its first; a nested crossing cannot precede the "+
				"call that made it", top-base)
	}
	return top, nil
}
