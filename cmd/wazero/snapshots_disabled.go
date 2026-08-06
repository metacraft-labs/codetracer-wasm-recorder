//go:build !ctsnapshots

// The open build. It records, replays and materialises exactly as the
// commercial build does; it derives no snapshots and seeks with none
// (`WASM-Replay-Snapshots-And-Slices.md` §9: "correctness is open, seek
// performance is commercial").
//
// This file does **not** import `internal/wasmsnapshot`. That is the whole
// point of the split: with no reference to it from `cmd/wazero`, the snapshot
// derivation and seek logic is not linked into the open artifact at all,
// rather than sitting inside it behind a runtime check. Snapshot spec §9
// rejects the runtime-plugin alternative for that reason.
//
// Two consequences follow, and both are properties of *this* file rather than
// of anything the open build does at run time:
//
//   - **A snapshot-bearing `.ct` is read normally.** The open build never
//     looks at the `snap*` namespaces, so their presence changes nothing: the
//     container's boundary streams are exactly what they would have been
//     without them. That is checked, byte for byte, by
//     `TestOpenBuildReadsSnapshotBearingContainer` in
//     `internal/boundarylog/seek_equivalence_test.go`.
//   - **The flags still exist**, and refuse with an explanation. An open build
//     that simply did not know `--snapshots` would leave a user unable to
//     distinguish "this build does not do that" from a typo.
package main

import (
	"fmt"
	"io"

	"github.com/tetratelabs/wazero/internal/boundarylog"
)

// snapshotsAvailable reports whether this build derives and consumes
// snapshots.
const snapshotsAvailable = false

func (o snapshotOptions) configure(
	rec *boundarylog.Recording,
	opts *boundarylog.Options,
	stdOut, stdErr io.Writer,
) (snapshotPlan, error) {
	if err := o.validate(); err != nil {
		return snapshotPlan{}, err
	}
	if o.requested() {
		return snapshotPlan{}, fmt.Errorf(
			"this build of the recorder does not derive or consume replay snapshots, " +
				"and does not split a recording into slices (every slice opens with a " +
				"base snapshot, so slicing is snapshot derivation). " +
				"It replays a boundary recording end to end and materialises a complete, " +
				"correct trace — including from a `.ct` that already carries snapshot " +
				"namespaces, which it reads and ignores. What it does not do is reach a " +
				"point in the middle without re-executing everything before it. Drop " +
				"--snapshots / --seek-from to materialise the recording linearly")
	}
	return snapshotPlan{finish: func(string) error { return nil }}, nil
}
