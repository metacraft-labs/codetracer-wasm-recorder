package wasmsnapshot

import (
	"errors"
	"fmt"

	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/ctfsffi"
)

// Builder accumulates snapshots and serialises them into the six `snap*`
// streams of snapshot spec §6.
//
// It is used only by the derivation path, which is gated behind the
// `ctsnapshots` build tag — but it lives in an untagged file because the
// stream encodings themselves are not commercial: the open build has to be
// able to *read* everything this writes, and keeping one encoder means the
// two builds can never drift apart in their idea of the format.
type Builder struct {
	points []QuiescentPoint
	tiers  *Tiers
	opts   EncodeOptions

	records []IndexRecord
	lay     []byte
	mem     []byte
	glob    []byte
	tab     []byte

	// bySeq maps a quiescent point ordinal to its index in `records`, so a
	// snapshot can be attached to the point it belongs to.
	bySeq map[int]int
}

// NewBuilder starts a snapshot set covering every quiescent point of a
// recording.
//
// Every point is listed in `snapshot.idx` whether or not it ends up carrying a
// snapshot. That is snapshot spec §8's second requirement — "the
// quiescent-point index … lets a consumer enumerate legal snapshot points
// without replaying" — and it means the snapshot *density* is a property of
// the data rather than of the format: re-deriving at a different density
// rewrites the flags, not the schema.
func NewBuilder(points []QuiescentPoint, tiers *Tiers, opts EncodeOptions) (*Builder, error) {
	b, err := NewIncrementalBuilder(tiers, opts)
	if err != nil {
		return nil, err
	}
	for _, p := range points {
		if err := b.AddPoint(p); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// NewIncrementalBuilder starts a snapshot set whose quiescent points are not
// known yet.
//
// This is the constructor the *streaming* pipeline of snapshot spec §2 needs.
// There, the boundary log is still arriving, so the recording's point count is
// unknowable until the page unloads — but a snapshot must be emitted at each
// point *as it is reached*, not in a pass at the end. Points are therefore
// declared with `AddPoint` as the replay discovers them.
//
// It is also what lets a slice (§7) carry an index covering only its own range
// rather than the whole recording's.
func NewIncrementalBuilder(tiers *Tiers, opts EncodeOptions) (*Builder, error) {
	if tiers == nil || tiers.PerTrace == nil {
		return nil, fmt.Errorf("wasmsnapshot: a snapshot builder needs a per-trace cache tier")
	}
	return &Builder{tiers: tiers, opts: opts, bySeq: map[int]int{}}, nil
}

// AddPoint declares one quiescent point, in ordinal order.
//
// Declaring a point is separate from snapshotting it (`Add`): every legal point
// is listed in `snapshot.idx` whether or not it carries data, which is what lets a
// consumer enumerate legal snapshot points without replaying (snapshot spec
// §8) and what makes snapshot *density* a property of the data rather than of
// the format.
func (b *Builder) AddPoint(p QuiescentPoint) error {
	if _, dup := b.bySeq[p.Ordinal]; dup {
		return fmt.Errorf(
			"wasmsnapshot: quiescent point %d is already in this snapshot index", p.Ordinal)
	}
	if n := len(b.points); n > 0 && p.Ordinal <= b.points[n-1].Ordinal {
		// The index is read back with a linear scan that stops at the first
		// record past the point being sought (`Nearest`), so an out-of-order
		// record would silently make earlier snapshots unreachable.
		return fmt.Errorf(
			"wasmsnapshot: quiescent point %d was declared after point %d; the "+
				"snapshot index must be in ascending ordinal order",
			p.Ordinal, b.points[n-1].Ordinal)
	}
	b.bySeq[p.Ordinal] = len(b.points)
	b.points = append(b.points, p)
	b.records = append(b.records, IndexRecord{
		Ordinal:       uint32(p.Ordinal),
		ExportsBefore: uint32(p.ExportsBefore),
		CrossingSeq:   int32(p.CrossingSeq),
	})
	return nil
}

// PointCount reports how many quiescent points the index lists.
func (b *Builder) PointCount() int { return len(b.points) }

// PayloadBytes reports how many bytes of snapshot data have accumulated.
//
// This is the quantity the slice-size policy of snapshot spec §7 is driven by;
// see `SlicePolicy.TargetBytes` for exactly what it does and does not include.
func (b *Builder) PayloadBytes() int64 {
	return int64(len(b.lay)+len(b.mem)+len(b.glob)+len(b.tab)) +
		b.tiers.PerTrace.Bytes()
}

// Add records a snapshot taken at one of the builder's quiescent points.
func (b *Builder) Add(s *Snapshot) error {
	i, ok := b.bySeq[s.Point]
	if !ok {
		return fmt.Errorf(
			"wasmsnapshot: snapshot claims quiescent point %d, which this recording "+
				"does not have (it has %d)", s.Point, len(b.points))
	}
	if b.records[i].HasSnapshot() {
		return fmt.Errorf("wasmsnapshot: quiescent point %d already carries a snapshot", s.Point)
	}

	regions, inline, err := EncodeMemory(s.Memory, b.tiers, 0, b.opts)
	if err != nil {
		return err
	}
	// `kind=0` file offsets are relative to the start of the whole
	// `snapshot.mem` stream, so rebase them onto what is already there.
	base := uint64(len(b.mem))
	for j := range regions {
		if regions[j].Kind == KindFull {
			regions[j].FileOffset += base
		}
	}
	b.mem = append(b.mem, inline...)

	layBytes := encodeRegions(regions)
	globBytes := encodeGlobals(s.Globals)
	tabBytes := encodeTables(s.Tables)

	b.records[i].Flags |= flagHasSnapshot
	b.records[i].MemoryBytes = s.MemoryBytes
	b.records[i].LayOffset, b.records[i].LayLength = uint64(len(b.lay)), uint64(len(layBytes))
	b.records[i].GlobOffset, b.records[i].GlobLength = uint64(len(b.glob)), uint64(len(globBytes))
	b.records[i].TabOffset, b.records[i].TabLength = uint64(len(b.tab)), uint64(len(tabBytes))
	b.lay = append(b.lay, layBytes...)
	b.glob = append(b.glob, globBytes...)
	b.tab = append(b.tab, tabBytes...)
	return nil
}

// MarkBase flags a quiescent point as a slice base (snapshot spec §7).
func (b *Builder) MarkBase(point int) error {
	i, ok := b.bySeq[point]
	if !ok {
		return fmt.Errorf("wasmsnapshot: quiescent point %d does not exist", point)
	}
	if !b.records[i].HasSnapshot() {
		return fmt.Errorf(
			"wasmsnapshot: quiescent point %d carries no snapshot, so it cannot be a "+
				"slice base — a slice is only independently materialisable because its "+
				"base is a complete resume point (snapshot spec §7)", point)
	}
	b.records[i].Flags |= flagBaseSnapshot
	return nil
}

// SnapshotCount reports how many quiescent points carry snapshot data.
func (b *Builder) SnapshotCount() int {
	n := 0
	for _, r := range b.records {
		if r.HasSnapshot() {
			n++
		}
	}
	return n
}

// Streams renders the six `snap*` namespace images.
func (b *Builder) Streams() map[string][]byte {
	return map[string][]byte{
		NamespaceIndex:     encodeIndex(b.records),
		NamespaceLayout:    b.lay,
		NamespaceInlineMem: b.mem,
		NamespacePages:     encodePageStore(b.tiers.PerTrace),
		NamespaceGlobals:   b.glob,
		NamespaceTables:    b.tab,
	}
}

// AttachTo appends the snapshot streams to an existing `.ct` container.
//
// This is snapshot spec §6 in one call: the streams go **inside** the trace
// container, as additive namespaces. Nothing is written outside it — that is
// what `verify_snapshots_are_not_sidecar_files` checks.
//
// The append happens in the canonical CTFS writer, through the FFI
// (`internal/ctfsffi`). It used to happen in a second implementation of the
// container layout carried here, which drifted from the canonical one on the
// multi-level block mapping and silently mis-wrote every stream past ~2 MB —
// `snapshot.mem` and `snappages.ns` cross that threshold for any real memory,
// so this call site is precisely where that bug landed. See the header of
// `internal/ctfs`.
func (b *Builder) AttachTo(containerPath string) error {
	return ctfsffi.Append(containerPath, b.Streams())
}

// ---------------------------------------------------------------------------
// Reading
// ---------------------------------------------------------------------------

// Set is a snapshot set read back from a container.
type Set struct {
	records []IndexRecord
	lay     []byte
	mem     []byte
	glob    []byte
	tab     []byte
	tiers   *Tiers
}

// Load reads the snapshot streams from a `.ct` container.
//
// The three-valued return is the narrowed version gate of snapshot spec §6:
//
//   - `(set, "", nil)` — snapshots are present and usable.
//   - `(nil, diagnostic, nil)` — no snapshots, or snapshots this build cannot
//     read. **This is not an error.** The container is a complete, valid
//     recording; the caller materialises it by linear replay and reports the
//     diagnostic. Refusing here would make a recording unreadable over a
//     component that only ever makes it faster.
//   - `(nil, "", err)` — the container itself is unreadable, or its snapshot
//     streams are structurally broken in a way that is not a version
//     difference. A corrupt snapshot stream is an error precisely because it
//     is *not* the additive case: it means bytes that claim to be version 1
//     do not parse as version 1, which no amount of linear replay explains.
func Load(containerPath string, useSystemCache bool) (*Set, string, error) {
	c, err := ctfs.Open(containerPath)
	if err != nil {
		return nil, "", err
	}
	if !c.Has(NamespaceIndex) {
		return nil, fmt.Sprintf(
			"%s carries no %s namespace: it is a complete recording without snapshots, "+
				"so any range will be materialised by linear replay",
			containerPath, NamespaceIndex), nil
	}

	raw := map[string][]byte{}
	for _, name := range NamespaceNames() {
		if !c.Has(name) {
			// `snapshot.idx` is present but a sibling is not. Treat this the same
			// way as an unknown version: seeking is unavailable, the recording
			// is not.
			return nil, fmt.Sprintf(
				"%s carries %s but not %s; the snapshot set is incomplete, so seeking "+
					"is unavailable and any range will be materialised by linear replay",
				containerPath, NamespaceIndex, name), nil
		}
		b, err := c.ReadFile(name)
		if err != nil {
			return nil, "", err
		}
		raw[name] = b
	}

	records, err := decodeIndex(raw[NamespaceIndex])
	if err != nil {
		var uv *UnsupportedVersionError
		if errors.As(err, &uv) {
			return nil, uv.Error(), nil
		}
		return nil, "", err
	}
	pages, err := decodePageStore(raw[NamespacePages])
	if err != nil {
		var uv *UnsupportedVersionError
		if errors.As(err, &uv) {
			return nil, uv.Error(), nil
		}
		return nil, "", err
	}

	s := &Set{
		records: records,
		lay:     raw[NamespaceLayout],
		mem:     raw[NamespaceInlineMem],
		glob:    raw[NamespaceGlobals],
		tab:     raw[NamespaceTables],
		tiers:   &Tiers{PerTrace: pages},
	}
	if useSystemCache {
		s.tiers.System = OpenSystemCache()
	}
	return s, "", nil
}

// Points returns the quiescent-point index — every legal snapshot point of the
// recording, whether or not it carries data.
func (s *Set) Points() []IndexRecord { return s.records }

// SnapshotCount reports how many points carry snapshot data.
func (s *Set) SnapshotCount() int {
	n := 0
	for _, r := range s.records {
		if r.HasSnapshot() {
			n++
		}
	}
	return n
}

// Range reports the quiescent-point range this snapshot index covers, and
// whether it opens with a base snapshot.
//
// For a whole-recording container that is 0..N and the base is point 0. For a
// **slice** (snapshot spec §7) it is the slice's own range: the index carries
// only that slice's points, the first of which is flagged as the base. That is
// what makes a slice self-describing — a consumer that fetched slice N alone,
// with no manifest and no sibling slices, can read off which range it holds and
// which point to resume from.
func (s *Set) Range() (base, end int, hasBase bool) {
	if len(s.records) == 0 {
		return 0, 0, false
	}
	first, last := s.records[0], s.records[len(s.records)-1]
	return int(first.Ordinal), int(last.Ordinal), first.IsBase() && first.HasSnapshot()
}

// Nearest returns the snapshot-bearing quiescent point at or before `point` —
// the seek target for materialising a range that starts there. It reports
// false when no snapshot precedes it, which means the range must be reached by
// linear replay from the beginning.
func (s *Set) Nearest(point int) (IndexRecord, bool) {
	var best IndexRecord
	found := false
	for _, r := range s.records {
		if int(r.Ordinal) > point {
			break
		}
		if r.HasSnapshot() {
			best, found = r, true
		}
	}
	return best, found
}

// Snapshot materialises one point's captured state.
func (s *Set) Snapshot(r IndexRecord) (*Snapshot, error) {
	if !r.HasSnapshot() {
		return nil, fmt.Errorf("wasmsnapshot: quiescent point %d carries no snapshot", r.Ordinal)
	}
	lay, err := sliceOf(s.lay, r.LayOffset, r.LayLength, NamespaceLayout)
	if err != nil {
		return nil, err
	}
	regions, err := decodeRegions(lay)
	if err != nil {
		return nil, err
	}
	memory, err := MaterialiseMemory(regions, s.mem, s.tiers, r.MemoryBytes)
	if err != nil {
		return nil, err
	}
	globRaw, err := sliceOf(s.glob, r.GlobOffset, r.GlobLength, NamespaceGlobals)
	if err != nil {
		return nil, err
	}
	globals, err := decodeGlobals(globRaw)
	if err != nil {
		return nil, err
	}
	tabRaw, err := sliceOf(s.tab, r.TabOffset, r.TabLength, NamespaceTables)
	if err != nil {
		return nil, err
	}
	tables, err := decodeTables(tabRaw)
	if err != nil {
		return nil, err
	}
	snap := &Snapshot{
		Point:       int(r.Ordinal),
		MemoryBytes: r.MemoryBytes,
		Globals:     globals,
		Tables:      tables,
	}
	if r.MemoryBytes > 0 {
		snap.Memory = memory
	}
	return snap, nil
}

func sliceOf(b []byte, off, length uint64, name string) ([]byte, error) {
	if off+length > uint64(len(b)) {
		return nil, fmt.Errorf(
			"wasmsnapshot: %s slice [%d,%d) runs past the %d-byte stream",
			name, off, off+length, len(b))
	}
	return b[off : off+length], nil
}

// Describe renders a table's reference type; kept here so `refTypeName` has a
// caller in every build configuration.
func (t TableValue) Describe() string {
	return fmt.Sprintf("%s table with %d element(s)", refTypeName(t.RefType), len(t.Elements))
}
