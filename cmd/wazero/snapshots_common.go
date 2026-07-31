package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// snapshotOptions carries the `--snapshots` / `--seek-*` command-line surface.
//
// The flags exist in **both** build variants on purpose. Snapshot spec §9
// makes seek performance the commercial capability and correctness the open
// one, and an open build that silently lacked the flags would leave a user
// unable to tell "this build does not do that" from "you typed it wrong". The
// open build accepts the flags and refuses them with an explanation; see
// `snapshots_disabled.go`.
type snapshotOptions struct {
	// derive turns on snapshot derivation during a `--boundary-log` replay.
	derive bool
	// every snapshots one quiescent point in `every` (1 = all of them).
	every int
	// useSystemCache consults the host-wide page CAS tier.
	useSystemCache bool
	// promote writes newly introduced pages into the host-wide tier.
	promote bool
	// source is the snapshot-bearing container to seek with.
	source string
	// from is the quiescent point to seek to, or seekUnset when no seek was
	// asked for. It is NOT defaulted to 0: point 0 is a real, legal seek
	// target (a snapshot of the freshly instantiated module), and conflating
	// "seek to the beginning" with "do not seek" would make the difference
	// between a snapshot-seeded and a linear materialisation unexpressible.
	from int
	// to is the quiescent point to stop at; 0 means "to the end of the
	// recording", which is unambiguous because a range ending at point 0
	// materialises nothing and is rejected by validate.
	to int

	// --- slices (WASM-Replay-Snapshots-And-Slices.md §7) ------------------

	// sliceDir is where slice containers and their manifest are written.
	// Empty means "do not slice", which is the default and stays a valid
	// configuration: §7's slice unit is "possibly absent entirely".
	sliceDir string
	// sliceEvery seals a slice every N quiescent points.
	sliceEvery int
	// sliceBytes seals a slice once its snapshot payload reaches N bytes.
	sliceBytes int64
	// sliceSnapshotEvery is the snapshot density *within* a slice, over and
	// above the base snapshot every slice carries. It is a separate knob from
	// the two above on purpose — see the INTERVAL vs SLICE note in
	// `internal/wasmsnapshot/slice.go`.
	sliceSnapshotEvery int
}

// snapshotPlan is what `configure` hands back to the boundary-log driver.
type snapshotPlan struct {
	// slicing reports that the trace is emitted as slice containers under
	// `--slice-dir` instead of as one container under `--out-dir`. The two are
	// alternatives rather than additions: a module has exactly one trace
	// recorder slot, and slicing swaps it per slice.
	slicing bool
	// finish runs once the replay has succeeded. `containerPath` is the
	// produced `.ct`, or "" when slicing (or when there is no `--out-dir`).
	finish func(containerPath string) error
}

// seekUnset is the `--seek-from` value meaning "no seek was requested".
const seekUnset = -1

// register declares the snapshot flags on a flag set.
func (o *snapshotOptions) register(flags *flag.FlagSet) {
	flags.BoolVar(&o.derive, "snapshots", false,
		"Derives replay snapshots during --boundary-log replay and stores them "+
			"inside the produced `.ct` as the wcp.* namespaces, so a later run can "+
			"materialise a sub-range without re-executing everything before it.  A "+
			"`.ct` without those namespaces is still a complete recording.")
	flags.IntVar(&o.every, "snapshot-every", 1,
		"With --snapshots, snapshots one quiescent point in `N` instead of all of "+
			"them.  Snapshots are derived data and can be re-derived at a different "+
			"density at any time without re-recording.")
	flags.BoolVar(&o.useSystemCache, "cas-system", false,
		"Consults the host-wide memory-page cache (CT_CAS_ROOT, default "+
			"~/.codetracer/cas) when deriving or replaying snapshots.")
	flags.BoolVar(&o.promote, "cas-share-system", false,
		"With --snapshots, also writes newly introduced memory pages into the "+
			"host-wide cache so later recordings on this host deduplicate against them.")
	flags.StringVar(&o.source, "snapshot-source", "",
		"Path to a snapshot-bearing `.ct` to seek with.  Required by --seek-from.")
	flags.IntVar(&o.from, "seek-from", seekUnset,
		"Materialises the recording starting at quiescent point `N`, reaching it "+
			"through the nearest preceding snapshot in --snapshot-source instead of "+
			"by re-executing everything before it.")
	flags.IntVar(&o.to, "seek-to", 0,
		"Stops materialising at quiescent point `N` (default: the end of the "+
			"recording).")
	flags.StringVar(&o.sliceDir, "slice-dir", "",
		"Emits the trace as a set of independently materialisable slice `.ct` "+
			"containers in this `directory`, plus a manifest, instead of one container "+
			"in --out-dir.  Each slice opens with a base snapshot, so a consumer can "+
			"fetch slice N alone and materialise its range without slices 0..N-1.  "+
			"Slices are sealed at quiescent points as the replay reaches them, so with "+
			"--boundary-stream they appear while the recording is still running.")
	flags.IntVar(&o.sliceEvery, "slice-every", 0,
		"With --slice-dir, seals a slice every `N` quiescent points (N exported "+
			"calls per slice).  0 disables the count trigger.")
	flags.Int64Var(&o.sliceBytes, "slice-bytes", 0,
		"With --slice-dir, seals a slice once its accumulated snapshot payload "+
			"reaches `N` bytes.  This measures the snapshot data, not the sealed "+
			"container: the trace half is buffered by the writer and cannot be sized "+
			"until it is produced.  0 disables the size trigger.  With neither trigger "+
			"set, the recording becomes a single slice.")
	flags.IntVar(&o.sliceSnapshotEvery, "slice-snapshot-every", 1,
		"With --slice-dir, snapshots one quiescent point in `N` inside each slice, "+
			"over and above the base snapshot every slice always carries.  This is the "+
			"seek granularity within a slice and is independent of slice size.")
}

// requested reports whether any snapshot behaviour was asked for.
func (o snapshotOptions) requested() bool {
	return o.derive || o.seeking() || o.to != 0 || o.source != "" || o.promote ||
		o.slicing()
}

// slicing reports whether slice production was asked for.
func (o snapshotOptions) slicing() bool { return o.sliceDir != "" }

// seeking reports whether a sub-range materialisation was asked for.
func (o snapshotOptions) seeking() bool { return o.from >= 0 }

// validate checks the flag combination independently of the build variant, so
// both artifacts reject the same nonsense with the same message.
func (o snapshotOptions) validate() error {
	if o.from < seekUnset || o.to < 0 {
		return fmt.Errorf("--seek-from and --seek-to are quiescent-point ordinals and cannot be negative")
	}
	if o.to != 0 && o.seeking() && o.to <= o.from {
		return fmt.Errorf(
			"--seek-to %d must be greater than --seek-from %d; a range that ends where "+
				"it starts materialises nothing", o.to, o.from)
	}
	if o.seeking() && o.source == "" {
		return fmt.Errorf(
			"--seek-from needs --snapshot-source <container.ct>: seeking reads the " +
				"snapshot namespaces of a previously produced trace container")
	}
	if o.source != "" && !o.seeking() {
		return fmt.Errorf("--snapshot-source has no effect without --seek-from")
	}
	if o.every < 1 {
		return fmt.Errorf("--snapshot-every must be at least 1")
	}
	if o.promote && !o.derive && !o.slicing() {
		return fmt.Errorf("--cas-share-system has no effect without --snapshots or --slice-dir")
	}
	if o.sliceEvery < 0 || o.sliceBytes < 0 {
		return fmt.Errorf("--slice-every and --slice-bytes cannot be negative")
	}
	if o.sliceSnapshotEvery < 1 {
		return fmt.Errorf("--slice-snapshot-every must be at least 1")
	}
	if !o.slicing() && (o.sliceEvery != 0 || o.sliceBytes != 0) {
		return fmt.Errorf(
			"--slice-every / --slice-bytes need --slice-dir: they choose where a slice " +
				"ends, which is only meaningful when slices are being written")
	}
	if o.slicing() && o.seeking() {
		return fmt.Errorf(
			"--slice-dir and --seek-from are opposite directions: one splits a " +
				"recording into slices as it is replayed, the other materialises a range " +
				"out of a container that already exists")
	}
	if o.slicing() && o.derive {
		return fmt.Errorf(
			"--slice-dir already derives snapshots — every slice opens with a base " +
				"snapshot, which is what makes it independently materialisable — so " +
				"--snapshots adds nothing. Use --slice-snapshot-every to set the " +
				"snapshot density inside a slice")
	}
	return nil
}

// containerPathFor locates the `.ct` the trace writer produced.
func containerPathFor(outDir, traceName string) (string, error) {
	base := filepath.Base(traceName)
	base = strings.TrimSuffix(base, filepath.Ext(base)) + ".ct"
	p := filepath.Join(outDir, base)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf(
			"expected the trace writer to produce %s, but it is not there: %w", p, err)
	}
	return p, nil
}
