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
}

// requested reports whether any snapshot behaviour was asked for.
func (o snapshotOptions) requested() bool {
	return o.derive || o.seeking() || o.to != 0 || o.source != "" || o.promote
}

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
	if o.promote && !o.derive {
		return fmt.Errorf("--cas-share-system has no effect without --snapshots")
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
