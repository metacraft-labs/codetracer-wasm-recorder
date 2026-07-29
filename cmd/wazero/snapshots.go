//go:build ctsnapshots

// Snapshot derivation and seeking — the commercial half of the build split
// specified in `WASM-Replay-Snapshots-And-Slices.md` §9.
//
// Snapshot spec §9 splits the feature by *producer build*, not by file:
//
//	                        | open | commercial
//	replays a recording     | yes  | yes
//	materialises a trace    | yes  | yes
//	derives snapshots       | no   | yes
//	reads a snapshot .ct    | yes, ignoring them | yes, seeking with them
//
// so "correctness is open, seek performance is commercial". This file is the
// only place `internal/wasmsnapshot` is reachable from the `wazero` binary,
// which is what keeps the derivation and seek logic out of the open artifact
// entirely rather than behind a runtime flag (§9 "Packaging" rejects the
// runtime-plugin seam for exactly that reason).
//
// Build the two artifacts with:
//
//	go build -o wazero            ./cmd/wazero   # open
//	go build -tags ctsnapshots -o wazero-snapshots ./cmd/wazero
package main

import (
	"fmt"
	"io"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/boundarylog"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/wasmsnapshot"
	"github.com/tetratelabs/wazero/tracewriter"
)

// snapshotsAvailable reports whether this build derives and consumes
// snapshots.
const snapshotsAvailable = true

// configure wires the requested snapshot behaviour into a replay.
//
// It returns a `finish` callback the caller invokes once the trace container
// exists on disk; that is where derived snapshots are attached, because they
// go *inside* the container (snapshot spec §6) and the container is only
// written when the replay has succeeded.
func (o snapshotOptions) configure(
	rec *boundarylog.Recording,
	opts *boundarylog.Options,
	stdOut, stdErr io.Writer,
) (plan snapshotPlan, err error) {
	if err := o.validate(); err != nil {
		return plan, err
	}

	var hooks []func(int, api.Module) error

	// Slice production (§7) needs no up-front point index: it declares each
	// quiescent point as the replay reaches it, which is what lets it work
	// against a recording that is still arriving (`--boundary-stream`).
	if o.slicing() {
		sw, err := o.sliceWriter(rec)
		if err != nil {
			return plan, err
		}
		hooks = append(hooks, sw.AtQuiescentPoint)
		opts.AtQuiescentPoint = chainHooks(hooks)
		// The slice writer owns the module's recorder slot from quiescent
		// point 0 onwards, so the whole-trace recorder must not also be
		// installed — two writers cannot both hold `ModuleInstance.Record`,
		// and a trace half-recorded into each would be neither.
		opts.Recorder = nil
		return snapshotPlan{slicing: true, finish: func(string) error {
			return finishSlices(sw, stdOut)
		}}, nil
	}

	// Everything below needs to know the recording's quiescent points up
	// front, which means the recording has to be complete. A streaming replay
	// is refused before it gets here (see `boundary_log.go`).
	points, err := wasmsnapshot.QuiescentPoints(rec)
	if err != nil {
		return plan, err
	}

	if o.seeking() {
		set, diag, err := wasmsnapshot.Load(o.source, true)
		if err != nil {
			return plan, err
		}
		if set == nil {
			// The narrowed version gate of snapshot spec §6: the container is
			// readable, seeking is not available. Say so and refuse the
			// *seek*, not the recording — the caller can drop --seek-from and
			// materialise the whole recording linearly.
			fmt.Fprintf(stdErr, "note: %s\n", diag)
			return plan, fmt.Errorf(
				"--seek-from was requested but %s carries no usable snapshots; drop "+
					"--seek-from to materialise the recording by linear replay", o.source)
		}
		nearest, ok := set.Nearest(o.from)
		if !ok {
			return plan, fmt.Errorf(
				"%s carries no snapshot at or before quiescent point %d, so that point "+
					"cannot be seeked to; it has %d snapshot(s) across %d point(s)",
				o.source, o.from, set.SnapshotCount(), len(set.Points()))
		}
		snap, err := set.Snapshot(nearest)
		if err != nil {
			return plan, err
		}
		if int(nearest.Ordinal) != o.from {
			// Seeking lands on the nearest preceding snapshot and replays
			// forward from there. Report it: the user asked for point N and
			// is getting the work of (N - ordinal) extra calls.
			fmt.Fprintf(stdOut,
				"seeking to quiescent point %d via the snapshot at point %d\n",
				o.from, nearest.Ordinal)
		}
		opts.FromPoint = int(nearest.Ordinal)
		opts.Resume = func(mod api.Module) error { return snap.Restore(mod) }
		if o.to > 0 {
			opts.ToPoint = o.to
		}
		if o.from > int(nearest.Ordinal) {
			// The caller wants the trace to start at `o.from`, not at the
			// snapshot: attach the recorder only once the replay gets there.
			recorder := opts.Recorder
			opts.Recorder = nil
			want := o.from
			hooks = append(hooks, func(p int, mod api.Module) error {
				if p != want {
					return nil
				}
				mi, ok := mod.(*wasm.ModuleInstance)
				if !ok {
					return fmt.Errorf("module is a %T, not a wazero ModuleInstance", mod)
				}
				mi.SetRecorder(recorder)
				return nil
			})
		}
	}

	var builder *wasmsnapshot.Builder
	if o.derive {
		builder, err = wasmsnapshot.NewBuilder(points,
			wasmsnapshot.NewTiers(o.useSystemCache),
			wasmsnapshot.EncodeOptions{PromoteToSystem: o.promote})
		if err != nil {
			return plan, err
		}
		every := o.every
		if every < 1 {
			every = 1
		}
		hooks = append(hooks, func(p int, mod api.Module) error {
			if p%every != 0 && p != len(points)-1 {
				return nil
			}
			snap, err := wasmsnapshot.Capture(mod, p)
			if err != nil {
				return err
			}
			return builder.Add(snap)
		})
	}

	if len(hooks) > 0 {
		opts.AtQuiescentPoint = chainHooks(hooks)
	}

	return snapshotPlan{finish: func(containerPath string) error {
		if builder == nil {
			return nil
		}
		if builder.SnapshotCount() == 0 {
			return fmt.Errorf(
				"--snapshots was requested but no snapshot was taken; the recording has " +
					"no quiescent point to snapshot at")
		}
		// Point 0 is always a slice base: a fresh instantiation is by
		// definition a complete resume point (snapshot spec §7).
		if err := builder.MarkBase(0); err != nil {
			return err
		}
		if err := builder.AttachTo(containerPath); err != nil {
			return err
		}
		fmt.Fprintf(stdOut, "attached %d snapshot(s) across %d quiescent point(s) to %s\n",
			builder.SnapshotCount(), len(points), containerPath)
		return nil
	}}, nil
}

// chainHooks runs several quiescent-point hooks as one, in order.
func chainHooks(hooks []func(int, api.Module) error) func(int, api.Module) error {
	return func(p int, mod api.Module) error {
		for _, h := range hooks {
			if err := h(p, mod); err != nil {
				return err
			}
		}
		return nil
	}
}

// sliceWriter builds the slice producer of snapshot spec §7 from the flags.
func (o snapshotOptions) sliceWriter(rec *boundarylog.Recording) (*wasmsnapshot.SliceWriter, error) {
	program := rec.Program
	if program == "" {
		// A recording still being streamed may not have had its
		// `trace_metadata.json` written yet. The name only decides what the
		// slice containers are called, so a stable fallback is better than
		// refusing to slice.
		program = "recording"
	}
	return wasmsnapshot.NewSliceWriter(wasmsnapshot.SliceOptions{
		Dir:     o.sliceDir,
		Program: program,
		Workdir: rec.Workdir,
		Policy: wasmsnapshot.SlicePolicy{
			EveryPoints: o.sliceEvery,
			TargetBytes: o.sliceBytes,
		},
		SnapshotEvery:  o.sliceSnapshotEvery,
		UseSystemCache: o.useSystemCache,
		Encode:         wasmsnapshot.EncodeOptions{PromoteToSystem: o.promote},
		NewRecorder:    func() tracewriter.TraceRecorder { return tracewriter.NewCtfsTraceWriter() },
	})
}

// finishSlices seals the last slice, writes the manifest and reports the set.
func finishSlices(sw *wasmsnapshot.SliceWriter, stdOut io.Writer) error {
	m, path, err := sw.Finish()
	if err != nil {
		return err
	}
	fmt.Fprintf(stdOut, "wrote %d slice(s) covering quiescent points 0..%d, manifest at %s\n",
		len(m.Slices), m.TotalPoints-1, path)
	for _, s := range m.Slices {
		fmt.Fprintf(stdOut, "  slice %d: points [%d,%d], %d call(s), %d snapshot(s), %d bytes -> %s\n",
			s.Index, s.BasePoint, s.EndPoint, s.ExportCalls, s.Snapshots, s.Bytes, s.File)
	}
	return nil
}
