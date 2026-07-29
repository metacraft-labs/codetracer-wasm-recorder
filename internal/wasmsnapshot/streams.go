package wasmsnapshot

import (
	"encoding/binary"
	"fmt"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/xxh3"
)

// SnapshotFormatVersion is the revision of the `wcp.*` stream encodings.
//
// Version gating follows CAS spec §10 with the deliberate narrowing snapshot
// spec §6 spells out: an unrecognised version **disables seeking and nothing
// else**. `Load` therefore reports an `*UnsupportedVersionError` as a
// diagnostic and the caller falls back to linear replay, instead of rejecting
// a recording it is perfectly able to materialise.
const SnapshotFormatVersion uint16 = 1

// CASFormat is the CAS spec §4 algorithm identifier, carried in `wcp.idx` so a
// future hash-family transition is detectable rather than silent.
const CASFormat = "page-xxh3-128"

// Namespace names, per snapshot spec §6. They mirror MCR's one-for-one so the
// two layouts stay legible side by side.
//
// # The base40 exception
//
// Snapshot spec §6 spells two of them `wcp.entry.lay` and `wcp.entry.mem`, and
// asserts the names "respect the base40 filename constraint of
// `CTFS-Binary-Format.md` §3". They do not: base40 packs exactly twelve
// characters into the u64 `FileEntry.Name`, MCR's `cp.entry.lay` is twelve
// exactly, and prefixing a `w` makes thirteen. The names below drop the first
// separator instead, which keeps the `wcp` prefix, keeps the `.lay` / `.mem`
// suffixes, and fits. This divergence from the spec text is recorded in the
// M38 completion note.
const (
	// NamespaceIndex is the snapshot / quiescent-point index.
	NamespaceIndex = "wcp.idx"
	// NamespaceLayout carries per-snapshot region descriptors
	// (MCR: `cp.entry.lay`).
	NamespaceLayout = "wcpentry.lay"
	// NamespaceInlineMem carries inline page bytes for `kind=0` regions
	// (MCR: `cp.entry.mem`).
	NamespaceInlineMem = "wcpentry.mem"
	// NamespacePages is the per-trace CAS tier (MCR: `cppages.ns`).
	NamespacePages = "wcppages.ns"
	// NamespaceGlobals carries global values per snapshot.
	NamespaceGlobals = "wcp.glob"
	// NamespaceTables carries table contents per snapshot.
	NamespaceTables = "wcp.tab"
)

// NamespaceNames lists every stream this package writes, in a stable order.
// A `.ct` carrying none of them is a complete, valid recording (snapshot spec
// §6); a reader that does not understand them replays linearly.
func NamespaceNames() []string {
	return []string{
		NamespaceIndex, NamespaceLayout, NamespaceInlineMem,
		NamespacePages, NamespaceGlobals, NamespaceTables,
	}
}

// UnsupportedVersionError is the narrowed version gate of snapshot spec §6: it
// means "seeking is unavailable", never "this recording is unreadable".
type UnsupportedVersionError struct {
	Stream  string
	Version uint16
}

func (e *UnsupportedVersionError) Error() string {
	return fmt.Sprintf(
		"snapshot stream %s is version %d; this build understands version %d. "+
			"Seeking is unavailable for this recording — it will be materialised by "+
			"linear replay instead. (The snapshot streams are additive and disjoint "+
			"from the boundary streams, so an unrecognised version disables seeking "+
			"and nothing else; see WASM-Replay-Snapshots-And-Slices.md §6.)",
		e.Stream, e.Version, SnapshotFormatVersion)
}

// ---------------------------------------------------------------------------
// `wcp.idx`
// ---------------------------------------------------------------------------

var indexMagic = [4]byte{'W', 'C', 'P', 'I'}

const (
	indexHeaderSize = 32
	indexRecordSize = 72
)

// Record flags.
const (
	// flagHasSnapshot marks a quiescent point that carries snapshot data.
	// Points without it are legal snapshot locations that this derivation
	// run chose not to snapshot — they are still listed, because snapshot
	// spec §8 wants a consumer to be able to enumerate legal points.
	flagHasSnapshot uint32 = 1 << 0
	// flagBaseSnapshot marks a slice boundary (snapshot spec §7). Written but
	// not yet acted upon; see the M38 completion note on slices.
	flagBaseSnapshot uint32 = 1 << 1
)

// IndexRecord is one `wcp.idx` entry: one quiescent point, with the offsets
// of its data in the other streams.
type IndexRecord struct {
	Ordinal       uint32
	ExportsBefore uint32
	CrossingSeq   int32
	Flags         uint32
	MemoryBytes   uint64
	LayOffset     uint64
	LayLength     uint64
	GlobOffset    uint64
	GlobLength    uint64
	TabOffset     uint64
	TabLength     uint64
}

// HasSnapshot reports whether this point carries snapshot data.
func (r IndexRecord) HasSnapshot() bool { return r.Flags&flagHasSnapshot != 0 }

// IsBase reports whether this point is a slice base (snapshot spec §7).
func (r IndexRecord) IsBase() bool { return r.Flags&flagBaseSnapshot != 0 }

func encodeIndex(records []IndexRecord) []byte {
	out := make([]byte, indexHeaderSize+len(records)*indexRecordSize)
	copy(out, indexMagic[:])
	binary.LittleEndian.PutUint16(out[4:], SnapshotFormatVersion)
	binary.LittleEndian.PutUint16(out[6:], 0) // flags, reserved
	binary.LittleEndian.PutUint32(out[8:], PageSize)
	binary.LittleEndian.PutUint32(out[12:], uint32(len(records)))
	copy(out[16:32], CASFormat)
	for i, r := range records {
		b := out[indexHeaderSize+i*indexRecordSize:]
		binary.LittleEndian.PutUint32(b[0:], r.Ordinal)
		binary.LittleEndian.PutUint32(b[4:], r.ExportsBefore)
		binary.LittleEndian.PutUint32(b[8:], uint32(r.CrossingSeq))
		binary.LittleEndian.PutUint32(b[12:], r.Flags)
		binary.LittleEndian.PutUint64(b[16:], r.MemoryBytes)
		binary.LittleEndian.PutUint64(b[24:], r.LayOffset)
		binary.LittleEndian.PutUint64(b[32:], r.LayLength)
		binary.LittleEndian.PutUint64(b[40:], r.GlobOffset)
		binary.LittleEndian.PutUint64(b[48:], r.GlobLength)
		binary.LittleEndian.PutUint64(b[56:], r.TabOffset)
		binary.LittleEndian.PutUint64(b[64:], r.TabLength)
	}
	return out
}

func decodeIndex(raw []byte) ([]IndexRecord, error) {
	if len(raw) < indexHeaderSize {
		return nil, fmt.Errorf("wasmsnapshot: %s is %d bytes, too short for its header",
			NamespaceIndex, len(raw))
	}
	if [4]byte(raw[0:4]) != indexMagic {
		return nil, fmt.Errorf("wasmsnapshot: %s does not carry the %q magic",
			NamespaceIndex, string(indexMagic[:]))
	}
	if v := binary.LittleEndian.Uint16(raw[4:]); v != SnapshotFormatVersion {
		return nil, &UnsupportedVersionError{Stream: NamespaceIndex, Version: v}
	}
	if ps := binary.LittleEndian.Uint32(raw[8:]); ps != PageSize {
		return nil, fmt.Errorf(
			"wasmsnapshot: %s addresses %d-byte pages; this build uses %d",
			NamespaceIndex, ps, PageSize)
	}
	if got := string(trimNul(raw[16:32])); got != CASFormat {
		return nil, fmt.Errorf(
			"wasmsnapshot: %s declares CAS format %q; this build implements %q",
			NamespaceIndex, got, CASFormat)
	}
	n := int(binary.LittleEndian.Uint32(raw[12:]))
	if indexHeaderSize+n*indexRecordSize != len(raw) {
		return nil, fmt.Errorf(
			"wasmsnapshot: %s declares %d record(s) but is %d bytes",
			NamespaceIndex, n, len(raw))
	}
	out := make([]IndexRecord, n)
	for i := range out {
		b := raw[indexHeaderSize+i*indexRecordSize:]
		out[i] = IndexRecord{
			Ordinal:       binary.LittleEndian.Uint32(b[0:]),
			ExportsBefore: binary.LittleEndian.Uint32(b[4:]),
			CrossingSeq:   int32(binary.LittleEndian.Uint32(b[8:])),
			Flags:         binary.LittleEndian.Uint32(b[12:]),
			MemoryBytes:   binary.LittleEndian.Uint64(b[16:]),
			LayOffset:     binary.LittleEndian.Uint64(b[24:]),
			LayLength:     binary.LittleEndian.Uint64(b[32:]),
			GlobOffset:    binary.LittleEndian.Uint64(b[40:]),
			GlobLength:    binary.LittleEndian.Uint64(b[48:]),
			TabOffset:     binary.LittleEndian.Uint64(b[56:]),
			TabLength:     binary.LittleEndian.Uint64(b[64:]),
		}
	}
	return out, nil
}

func trimNul(b []byte) []byte {
	for i, c := range b {
		if c == 0 {
			return b[:i]
		}
	}
	return b
}

// ---------------------------------------------------------------------------
// `wcpentry.lay` — region descriptors (CAS spec §3)
// ---------------------------------------------------------------------------

func encodeRegions(regions []Region) []byte {
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, uint32(len(regions)))
	var scratch [8]byte
	put64 := func(v uint64) {
		binary.LittleEndian.PutUint64(scratch[:], v)
		out = append(out, scratch[:]...)
	}
	put32 := func(v uint32) {
		binary.LittleEndian.PutUint32(scratch[:4], v)
		out = append(out, scratch[:4]...)
	}
	putHashes := func(hs []xxh3.Uint128) {
		for _, h := range hs {
			b := h.Bytes()
			out = append(out, b[:]...)
		}
	}
	for _, r := range regions {
		put64(r.Base)
		put64(r.Size)
		put32(r.Protect)
		put32(r.Flags)
		out = append(out, r.Kind, 0, 0, 0) // kind + reserved[3], MUST be zero
		switch r.Kind {
		case KindFull:
			put64(r.FileOffset)
		case KindHashRef:
			put32(uint32(len(r.Hashes)))
			putHashes(r.Hashes)
		case KindRunHashRef:
			put32(uint32(len(r.Runs)))
			for _, run := range r.Runs {
				put64(run.CacheID)
				put32(uint32(len(run.Hashes)))
				putHashes(run.Hashes)
			}
		}
	}
	return out
}

// regionReader is a bounds-checked cursor. Every field read goes through it so
// a truncated stream produces a named error instead of a panic.
type regionReader struct {
	b   []byte
	at  int
	err error
}

func (r *regionReader) need(n int) []byte {
	if r.err != nil {
		return make([]byte, n)
	}
	if r.at+n > len(r.b) {
		r.err = fmt.Errorf(
			"wasmsnapshot: %s is truncated: %d more byte(s) needed at offset %d of %d",
			NamespaceLayout, n, r.at, len(r.b))
		return make([]byte, n)
	}
	s := r.b[r.at : r.at+n]
	r.at += n
	return s
}

func (r *regionReader) u32() uint32 { return binary.LittleEndian.Uint32(r.need(4)) }
func (r *regionReader) u64() uint64 { return binary.LittleEndian.Uint64(r.need(8)) }

func (r *regionReader) hashes(n uint32) ([]xxh3.Uint128, error) {
	// Guard before allocating: a corrupt count must not turn into a
	// multi-gigabyte allocation.
	if uint64(n)*16 > uint64(len(r.b)-r.at) {
		return nil, fmt.Errorf(
			"wasmsnapshot: %s declares %d page hash(es) but only %d byte(s) remain",
			NamespaceLayout, n, len(r.b)-r.at)
	}
	out := make([]xxh3.Uint128, n)
	for i := range out {
		var hb [16]byte
		copy(hb[:], r.need(16))
		out[i] = xxh3.Uint128FromBytes(hb)
	}
	return out, r.err
}

func decodeRegions(raw []byte) ([]Region, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	r := &regionReader{b: raw}
	n := r.u32()
	if r.err != nil {
		return nil, r.err
	}
	if uint64(n)*32 > uint64(len(raw)) {
		return nil, fmt.Errorf(
			"wasmsnapshot: %s declares %d region(s), more than %d bytes can hold",
			NamespaceLayout, n, len(raw))
	}
	regions := make([]Region, 0, n)
	for i := uint32(0); i < n; i++ {
		var reg Region
		reg.Base = r.u64()
		reg.Size = r.u64()
		reg.Protect = r.u32()
		reg.Flags = r.u32()
		kb := r.need(4)
		reg.Kind = kb[0]
		if kb[1] != 0 || kb[2] != 0 || kb[3] != 0 {
			return nil, fmt.Errorf(
				"wasmsnapshot: %s region %d has non-zero reserved bytes; CAS spec §3 "+
					"requires them to be zero", NamespaceLayout, i)
		}
		if r.err != nil {
			return nil, r.err
		}
		switch reg.Kind {
		case KindFull:
			reg.FileOffset = r.u64()
		case KindHashRef:
			c := r.u32()
			if r.err != nil {
				return nil, r.err
			}
			hs, err := r.hashes(c)
			if err != nil {
				return nil, err
			}
			reg.Hashes = hs
		case KindRunHashRef:
			runs := r.u32()
			if r.err != nil {
				return nil, r.err
			}
			if uint64(runs)*12 > uint64(len(raw)-r.at) {
				return nil, fmt.Errorf(
					"wasmsnapshot: %s region %d declares %d run(s), more than the "+
						"remaining bytes can hold", NamespaceLayout, i, runs)
			}
			for j := uint32(0); j < runs; j++ {
				var run HashRun
				run.CacheID = r.u64()
				c := r.u32()
				if r.err != nil {
					return nil, r.err
				}
				hs, err := r.hashes(c)
				if err != nil {
					return nil, err
				}
				run.Hashes = hs
				reg.Runs = append(reg.Runs, run)
			}
		default:
			return nil, fmt.Errorf(
				"wasmsnapshot: %s region %d has unknown kind %d", NamespaceLayout, i, reg.Kind)
		}
		if r.err != nil {
			return nil, r.err
		}
		regions = append(regions, reg)
	}
	return regions, nil
}

// ---------------------------------------------------------------------------
// `wcp.glob` and `wcp.tab`
// ---------------------------------------------------------------------------

const globalRecordSize = 20

func encodeGlobals(gs []GlobalValue) []byte {
	out := make([]byte, 4+len(gs)*globalRecordSize)
	binary.LittleEndian.PutUint32(out, uint32(len(gs)))
	for i, g := range gs {
		b := out[4+i*globalRecordSize:]
		b[0] = g.ValueType
		if g.Mutable {
			b[1] = 1
		}
		binary.LittleEndian.PutUint64(b[4:], g.Lo)
		binary.LittleEndian.PutUint64(b[12:], g.Hi)
	}
	return out
}

func decodeGlobals(raw []byte) ([]GlobalValue, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) < 4 {
		return nil, fmt.Errorf("wasmsnapshot: %s slice is %d bytes", NamespaceGlobals, len(raw))
	}
	n := int(binary.LittleEndian.Uint32(raw))
	if 4+n*globalRecordSize != len(raw) {
		return nil, fmt.Errorf(
			"wasmsnapshot: %s slice declares %d global(s) but is %d bytes",
			NamespaceGlobals, n, len(raw))
	}
	out := make([]GlobalValue, n)
	for i := range out {
		b := raw[4+i*globalRecordSize:]
		out[i] = GlobalValue{
			ValueType: api.ValueType(b[0]),
			Mutable:   b[1] != 0,
			Lo:        binary.LittleEndian.Uint64(b[4:]),
			Hi:        binary.LittleEndian.Uint64(b[12:]),
		}
	}
	return out, nil
}

func encodeTables(ts []TableValue) []byte {
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, uint32(len(ts)))
	var scratch [4]byte
	for _, t := range ts {
		out = append(out, t.RefType, 0, 0, 0)
		binary.LittleEndian.PutUint32(scratch[:], uint32(len(t.Elements)))
		out = append(out, scratch[:]...)
		for _, e := range t.Elements {
			binary.LittleEndian.PutUint32(scratch[:], uint32(e))
			out = append(out, scratch[:]...)
		}
	}
	return out
}

func decodeTables(raw []byte) ([]TableValue, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) < 4 {
		return nil, fmt.Errorf("wasmsnapshot: %s slice is %d bytes", NamespaceTables, len(raw))
	}
	n := int(binary.LittleEndian.Uint32(raw))
	at := 4
	out := make([]TableValue, 0, n)
	for i := 0; i < n; i++ {
		if at+8 > len(raw) {
			return nil, fmt.Errorf("wasmsnapshot: %s slice is truncated at table %d", NamespaceTables, i)
		}
		t := TableValue{RefType: raw[at]}
		count := int(binary.LittleEndian.Uint32(raw[at+4:]))
		at += 8
		if count < 0 || at+count*4 > len(raw) {
			return nil, fmt.Errorf(
				"wasmsnapshot: %s table %d declares %d element(s) but the slice is %d bytes",
				NamespaceTables, i, count, len(raw))
		}
		t.Elements = make([]int32, count)
		for j := 0; j < count; j++ {
			t.Elements[j] = int32(binary.LittleEndian.Uint32(raw[at:]))
			at += 4
		}
		out = append(out, t)
	}
	if at != len(raw) {
		return nil, fmt.Errorf(
			"wasmsnapshot: %s slice has %d trailing byte(s)", NamespaceTables, len(raw)-at)
	}
	return out, nil
}

// refTypeName renders a table's reference type for a diagnostic.
func refTypeName(t byte) string {
	switch t {
	case wasm.RefTypeFuncref:
		return "funcref"
	case wasm.RefTypeExternref:
		return "externref"
	default:
		return fmt.Sprintf("reftype %#x", t)
	}
}
