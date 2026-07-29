package wasmsnapshot

import (
	"fmt"

	"github.com/tetratelabs/wazero/internal/boundarylog"
)

// QuiescentPoint is one legal snapshot location: a moment when no exported
// function is executing (snapshot spec §3).
//
// For a recording with N top-level exported calls there are exactly N+1
// quiescent points — one before each call and one after the last. Point 0 is
// the freshly instantiated module; point N is the end of the recording.
type QuiescentPoint struct {
	// Ordinal is the point's index, 0..N.
	Ordinal int
	// ExportsBefore is the number of top-level exported calls completed
	// before this point. It equals Ordinal, and is carried separately so the
	// on-disk record stays legible if the two ever diverge (e.g. if a future
	// version admits mid-call points, snapshot spec §11).
	ExportsBefore int
	// CrossingSeq is the boundary-event ordinal of the *next* crossing the
	// recording expects — i.e. the `Crossing.Seq` of the exported call this
	// point precedes. It is -1 at the final point, where the recording is
	// exhausted.
	//
	// This is the value a resuming replay seeds its crossing cursor with, so
	// import crossings keep lining up with the recording after a seek.
	CrossingSeq int
	// NextExport names the exported function this point precedes, or "" at
	// the final point. Diagnostic only.
	NextExport string
}

// QuiescentPoints enumerates a recording's quiescent points.
//
// It reads only the boundary log — no replay, no module. That is the property
// snapshot spec §3 calls out ("trivially identified from the boundary log
// alone — the log already delimits every exported call") and §8 turns into a
// deliverable ("the quiescent-point index … lets a consumer enumerate legal
// snapshot points without replaying").
//
// Nested export crossings (a host callback re-entering the module) do **not**
// create quiescent points: while one is open an exported function *is*
// executing. `boundarylog.Replay` refuses such recordings outright, so this
// function reports them rather than silently producing a point index that
// would place a snapshot mid-call.
func QuiescentPoints(rec *boundarylog.Recording) ([]QuiescentPoint, error) {
	if rec == nil {
		return nil, fmt.Errorf("wasmsnapshot: no recording supplied")
	}
	if nested := rec.NestedExports(); len(nested) > 0 {
		return nil, fmt.Errorf(
			"wasmsnapshot: the recording contains %d nested exported call(s); while "+
				"one is open an exported function is executing, so the points between "+
				"top-level calls are not quiescent (snapshot spec §3) and no snapshot "+
				"may be taken", len(nested))
	}
	exports := rec.TopLevelExports()
	points := make([]QuiescentPoint, 0, len(exports)+1)
	for i, c := range exports {
		points = append(points, QuiescentPoint{
			Ordinal:       i,
			ExportsBefore: i,
			CrossingSeq:   c.Seq,
			NextExport:    c.Name,
		})
	}
	points = append(points, QuiescentPoint{
		Ordinal:       len(exports),
		ExportsBefore: len(exports),
		CrossingSeq:   -1,
	})
	return points, nil
}
