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
) (finish func(containerPath string) error, err error) {
	if err := o.validate(); err != nil {
		return nil, err
	}

	points, err := wasmsnapshot.QuiescentPoints(rec)
	if err != nil {
		return nil, err
	}
	var hooks []func(int, api.Module) error

	if o.seeking() {
		set, diag, err := wasmsnapshot.Load(o.source, true)
		if err != nil {
			return nil, err
		}
		if set == nil {
			// The narrowed version gate of snapshot spec §6: the container is
			// readable, seeking is not available. Say so and refuse the
			// *seek*, not the recording — the caller can drop --seek-from and
			// materialise the whole recording linearly.
			fmt.Fprintf(stdErr, "note: %s\n", diag)
			return nil, fmt.Errorf(
				"--seek-from was requested but %s carries no usable snapshots; drop "+
					"--seek-from to materialise the recording by linear replay", o.source)
		}
		nearest, ok := set.Nearest(o.from)
		if !ok {
			return nil, fmt.Errorf(
				"%s carries no snapshot at or before quiescent point %d, so that point "+
					"cannot be seeked to; it has %d snapshot(s) across %d point(s)",
				o.source, o.from, set.SnapshotCount(), len(set.Points()))
		}
		snap, err := set.Snapshot(nearest)
		if err != nil {
			return nil, err
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
				mi.Record = recorder
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
			return nil, err
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
		opts.AtQuiescentPoint = func(p int, mod api.Module) error {
			for _, h := range hooks {
				if err := h(p, mod); err != nil {
					return err
				}
			}
			return nil
		}
	}

	return func(containerPath string) error {
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
	}, nil
}
