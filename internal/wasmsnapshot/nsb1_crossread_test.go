package wasmsnapshot

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/tetratelabs/wazero/internal/xxh3"
)

// `wcppages.ns` is a real `NSB1` namespace B-tree, and this file is what makes
// that claim checkable rather than asserted.
//
// A round-trip test through `encodePageStore` / `decodePageStore` cannot catch
// a mistake in the page layout, because it writes and reads with the same
// convention — exactly the trap `internal/ctfs/multilevel_layout_test.go`
// documents for the container's block mapping. So, as there, the writer is
// held to a **transcription of the producer's own reader**:
// `nimLoadCowBTree` / `nimLookup` below are `loadCowBTree` and `lookupFrom`
// from `codetracer-trace-format-nim/src/codetracer_ctfs/cow_btree.nim`,
// transcribed. They share no code with `nsb1.go`, so a namespace this package
// writes has to satisfy the producer's algorithm rather than its own.
//
// Kept deliberately literal so it stays diffable against that source: the same
// header offsets, the same "highest valid commit id wins" root selection, the
// same `lowerBound`, and — the subtle part — the same asymmetric descent, in
// which an internal node whose key *equals* the search key sends the walk into
// the child to its **right**, because a separator is a subtree minimum and not
// an exclusive upper bound (`CTFS-Binary-Format.md` §10).
//
// The second, independent proof is `nsb1_nim_crossread_test.go`, which runs
// the real Nim reader rather than a transcription of it.
//
// NO MOCKS. Every image below is produced by the production encoder from real
// pages and real xxh3-128 hashes.

// nimCowBTree is `CowBTree` reduced to what a reader needs.
type nimCowBTree struct {
	descriptorSize   int
	pages            []byte
	root0, root1     uint64
	commit0, commit1 uint64
}

// Transcribed constants: the `NamespaceHeader` field offsets and the node
// header size, from `cow_btree.nim`.
const (
	nimOffRoot0        = 4
	nimOffRoot1        = 12
	nimOffCommit0      = 20
	nimOffCommit1      = 28
	nimOffFlags        = 36
	nimHeaderTotal     = 61
	nimNodeHeaderBytes = 8
	nimPageSize        = 4096
	nimKindLeaf        = 1
)

// Transcribed leaf types.
const (
	nimCltTypeA = 0
	nimCltTypeB = 1
)

func nimRU16(buf []byte, off int) uint16 {
	return uint16(buf[off]) | (uint16(buf[off+1]) << 8)
}

func nimRU64(buf []byte, off int) uint64 {
	var r uint64
	for i := 0; i < 8; i++ {
		r |= uint64(buf[off+i]) << (i * 8)
	}
	return r
}

// nimLoadCowBTree is `loadCowBTree`, transcribed.
func nimLoadCowBTree(image []byte, leafType int) (*nimCowBTree, error) {
	if len(image) < nimHeaderTotal {
		return nil, fmt.Errorf("image too short for namespace header")
	}
	if image[0] != 'N' || image[1] != 'S' || image[2] != 'B' || image[3] != '1' {
		return nil, fmt.Errorf("invalid namespace B-tree magic")
	}
	if len(image)%nimPageSize != 0 {
		return nil, fmt.Errorf("image not page-aligned")
	}
	descSize := 8
	if leafType == nimCltTypeB {
		descSize = 16
	}
	return &nimCowBTree{
		descriptorSize: descSize,
		pages:          image,
		root0:          nimRU64(image, nimOffRoot0),
		root1:          nimRU64(image, nimOffRoot1),
		commit0:        nimRU64(image, nimOffCommit0),
		commit1:        nimRU64(image, nimOffCommit1),
	}, nil
}

// nimCommittedSlot is `committedSlot`, transcribed.
func (t *nimCowBTree) nimCommittedSlot() int {
	if t.commit0 == 0 && t.commit1 == 0 {
		return -1
	}
	if t.commit1 > t.commit0 {
		return 1
	}
	return 0
}

// nimCommittedRoot is `committedRoot`, transcribed.
func (t *nimCowBTree) nimCommittedRoot() uint64 {
	s := t.nimCommittedSlot()
	switch {
	case s < 0:
		return 0
	case s == 0:
		return t.root0
	default:
		return t.root1
	}
}

func nimPageBase(page uint64) int { return int(page) * nimPageSize }

func (t *nimCowBTree) nimNodeIsLeaf(page uint64) bool {
	return t.pages[nimPageBase(page)] == nimKindLeaf
}

func (t *nimCowBTree) nimNodeCount(page uint64) int {
	return int(nimRU16(t.pages, nimPageBase(page)+2))
}

func (t *nimCowBTree) nimNodeKey(page uint64, i int) uint64 {
	return nimRU64(t.pages, nimPageBase(page)+nimNodeHeaderBytes+i*8)
}

func (t *nimCowBTree) nimLeafDescOffset(page uint64, count, i int) int {
	return nimPageBase(page) + nimNodeHeaderBytes + count*8 + i*t.descriptorSize
}

func (t *nimCowBTree) nimNodeChild(page uint64, count, i int) uint64 {
	return nimRU64(t.pages, nimPageBase(page)+nimNodeHeaderBytes+count*8+i*8)
}

// nimLowerBound is `lowerBound`, transcribed.
func (t *nimCowBTree) nimLowerBound(page uint64, count int, key uint64) int {
	lo, hi := 0, count
	for lo < hi {
		mid := (lo + hi) >> 1
		if t.nimNodeKey(page, mid) < key {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return lo
}

// nimLookupFrom is `lookupFrom`, transcribed.
func (t *nimCowBTree) nimLookupFrom(root, key uint64) ([]byte, error) {
	if root == 0 {
		return nil, fmt.Errorf("key not found")
	}
	page := root
	for {
		count := t.nimNodeCount(page)
		idx := t.nimLowerBound(page, count, key)
		if t.nimNodeIsLeaf(page) {
			if idx < count && t.nimNodeKey(page, idx) == key {
				off := t.nimLeafDescOffset(page, count, idx)
				return t.pages[off : off+t.descriptorSize], nil
			}
			return nil, fmt.Errorf("key not found")
		}
		childIdx := idx
		if idx < count && t.nimNodeKey(page, idx) == key {
			childIdx = idx + 1
		}
		page = t.nimNodeChild(page, count, childIdx)
	}
}

// nimLookup is `lookup`, transcribed.
func (t *nimCowBTree) nimLookup(key uint64) ([]byte, error) {
	return t.nimLookupFrom(t.nimCommittedRoot(), key)
}

// ---------------------------------------------------------------------------
// The proofs
// ---------------------------------------------------------------------------

// TestTheProducersTraversalFindsEveryPageInTheStore is the deliverable: an
// independent reader, following the producer's own algorithm, can look up
// every page of a `wcppages.ns` by its CAS spec §5.4 truncated key and get the
// right bytes back.
//
// 400 distinct pages force a multi-level tree — Leaf Type B holds 170 keys per
// leaf, so the reader must descend an internal node to reach most of them,
// which is the half a single-leaf store would leave untested.
func TestTheProducersTraversalFindsEveryPageInTheStore(t *testing.T) {
	const n = 400

	c := NewPerTraceCache()
	hashes := make([]xxh3.Uint128, 0, n)
	for i := int64(1); i <= n; i++ {
		p := page(i)
		h := HashPage(p)
		if err := c.Insert(h, p); err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, h)
	}
	raw := encodePageStore(c)

	tree, err := nimLoadCowBTree(raw, nimCltTypeB)
	if err != nil {
		t.Fatalf("the producer's reader rejected the stream: %v", err)
	}
	if tree.nimCommittedRoot() == 0 {
		t.Fatal("the producer's reader found no committed root")
	}
	// A single-level tree would leave the internal-node descent untested, so
	// assert the shape the key count implies rather than assume it.
	if tree.nimNodeIsLeaf(tree.nimCommittedRoot()) {
		t.Fatalf("%d keys produced a single-leaf tree; the internal-node descent "+
			"is not exercised", n)
	}

	for _, h := range hashes {
		desc, err := tree.nimLookup(h.Lo)
		if err != nil {
			t.Fatalf("the producer's reader did not find key %#x: %v", h.Lo, err)
		}
		if len(desc) != pageStoreDescSize {
			t.Fatalf("key %#x resolved to a %d-byte descriptor, want %d",
				h.Lo, len(desc), pageStoreDescSize)
		}
		// Resolve the descriptor exactly as `linehits_builder.nim`'s own
		// `decodeCowLinehitsPayloadForTest` does: an absolute offset and a
		// length into the same image.
		start := binary.LittleEndian.Uint64(desc[0:])
		length := binary.LittleEndian.Uint64(desc[8:])
		if start+length > uint64(len(raw)) {
			t.Fatalf("key %#x points at bytes [%d,%d) of a %d-byte stream",
				h.Lo, start, start+length, len(raw))
		}
		rec := raw[start : start+length]
		if len(rec) != pageRecordHeaderSize+PageSize {
			t.Fatalf("key %#x has a %d-byte payload, want one %d-byte CAS entry",
				h.Lo, len(rec), pageRecordHeaderSize+PageSize)
		}
		var hb [16]byte
		copy(hb[:], rec[0:16])
		if got := xxh3.Uint128FromBytes(hb); got != h {
			t.Fatalf("key %#x carries full hash %x, want %x", h.Lo, got, h)
		}
		if sz := binary.LittleEndian.Uint32(rec[16:]); sz != PageSize {
			t.Fatalf("key %#x declares a %d-byte page", h.Lo, sz)
		}
		want, _ := c.Lookup(h)
		if !bytes.Equal(rec[pageRecordHeaderSize:], want) {
			t.Fatalf("key %#x resolved to the wrong page bytes", h.Lo)
		}
	}

	// Negative control: without it, a reader that returned the first leaf
	// entry for every query would pass everything above.
	present := map[uint64]bool{}
	for _, h := range hashes {
		present[h.Lo] = true
	}
	absent := 0
	for k := uint64(1); absent < 64 && k < 1<<20; k++ {
		if present[k] {
			continue
		}
		if _, err := tree.nimLookup(k); err == nil {
			t.Fatalf("the producer's reader resolved key %#x, which is not in the store", k)
		}
		absent++
	}
	if absent == 0 {
		t.Fatal("the negative control checked no absent key")
	}
}

// TestTheProducersTraversalReadsADeepBulkLoadedTree exercises the interior
// levels the page store itself cannot reach.
//
// A `wcppages.ns` deep enough to have three levels would need 29 071 keys and
// therefore 1.9 GiB of 64 KiB pages, so the tree shape is exercised here at
// the `nsb1.go` layer instead, over synthetic descriptors. Non-contiguous keys
// (`i*7 + 3`) keep the separators from coinciding with a positional index, so
// an off-by-one in the copy-up rule cannot go unnoticed.
func TestTheProducersTraversalReadsADeepBulkLoadedTree(t *testing.T) {
	// 60 000 keys over an order-170 tree gives 354 leaves, 3 internal nodes and
	// a root — three levels.
	const n = 60000
	entries := make([]nsEntry, n)
	keys := make([]uint64, n)
	for i := 0; i < n; i++ {
		k := uint64(i)*7 + 3
		keys[i] = k
		d := make([]byte, nsDescSizeTypeB)
		binary.LittleEndian.PutUint64(d[0:], k*11+1)
		binary.LittleEndian.PutUint64(d[8:], ^k)
		entries[i] = nsEntry{key: k, desc: d}
	}
	image, err := nsBulkLoad(entries, nsDescSizeTypeB, pageStoreFlags)
	if err != nil {
		t.Fatalf("nsBulkLoad: %v", err)
	}

	tree, err := nimLoadCowBTree(image, nimCltTypeB)
	if err != nil {
		t.Fatalf("the producer's reader rejected the image: %v", err)
	}
	// Depth is asserted, not assumed: an order-170 tree of 60 000 keys is three
	// levels, and a shallower one would mean this test covers less than it
	// claims.
	depth := 1
	for p := tree.nimCommittedRoot(); !tree.nimNodeIsLeaf(p); depth++ {
		p = tree.nimNodeChild(p, tree.nimNodeCount(p), 0)
	}
	if depth != 3 {
		t.Fatalf("the tree is %d level(s) deep, want 3", depth)
	}

	for i, k := range keys {
		got, err := tree.nimLookup(k)
		if err != nil {
			t.Fatalf("key %#x (entry %d) was not found: %v", k, i, err)
		}
		if !bytes.Equal(got, entries[i].desc) {
			t.Fatalf("key %#x resolved to %x, want %x", k, got, entries[i].desc)
		}
	}
	// Negative control: `i*7 + 3` leaves six unused keys between neighbours,
	// every one of which must miss.
	for i := 0; i < n; i += 997 {
		for d := uint64(1); d < 7; d++ {
			absent := uint64(i)*7 + 3 + d
			if _, err := tree.nimLookup(absent); err == nil {
				t.Fatalf("the producer's reader resolved key %#x, which was never inserted",
					absent)
			}
		}
	}
}

// TestAnUnknownPageStoreVersionIsADiagnostic pins the behaviour the `NSB1`
// switch had to preserve: the namespace format has no version field of its
// own, so `SnapshotFormatVersion` rides in page 0's unused tail, and a stream
// this build does not understand must still downgrade seeking rather than
// condemn the recording.
func TestAnUnknownPageStoreVersionIsADiagnostic(t *testing.T) {
	c := NewPerTraceCache()
	p := page(1)
	if err := c.Insert(HashPage(p), p); err != nil {
		t.Fatal(err)
	}
	raw := encodePageStore(c)
	binary.LittleEndian.PutUint16(raw[pageStoreSidecarOffset+4:], SnapshotFormatVersion+1)

	_, err := decodePageStore(raw)
	uv, ok := err.(*UnsupportedVersionError)
	if !ok {
		t.Fatalf("an unknown page-store version produced %T: %v", err, err)
	}
	if uv.Stream != NamespacePages {
		t.Errorf("the diagnostic names stream %q", uv.Stream)
	}
	if !bytes.Contains([]byte(uv.Error()), []byte("linear replay")) {
		t.Errorf("the diagnostic does not say what happens instead: %v", uv)
	}
	// And the stream really is a valid namespace regardless — the version is
	// carried beside the format, not inside it.
	if _, err := nimLoadCowBTree(raw, nimCltTypeB); err != nil {
		t.Errorf("the producer's reader rejected a version-bumped stream: %v", err)
	}
}

// TestTruncatedPageStoresAreReportedNotPanicked: every prefix of a real stream
// must produce an error or an empty tier, never a panic and never a page.
func TestTruncatedPageStoresAreReportedNotPanicked(t *testing.T) {
	c := NewPerTraceCache()
	for i := int64(1); i <= 3; i++ {
		p := page(i)
		if err := c.Insert(HashPage(p), p); err != nil {
			t.Fatal(err)
		}
	}
	raw := encodePageStore(c)
	// Every page boundary, plus a scattering of unaligned cuts.
	cuts := []int{1, 60, 61, 63, 100, nsPageSize - 1, nsPageSize + 1}
	for off := 0; off < len(raw); off += nsPageSize {
		cuts = append(cuts, off, off+17)
	}
	for _, cut := range cuts {
		if cut <= 0 || cut >= len(raw) {
			continue
		}
		back, err := decodePageStoreKeyed(raw[:cut], truncatedKey)
		if err != nil {
			continue
		}
		if back.Len() != 0 {
			t.Fatalf("a stream truncated to %d of %d bytes decoded %d page(s)",
				cut, len(raw), back.Len())
		}
	}
}

// TestATruncatedKeyCollisionIsResolvedByTheFullHash drives the one case real
// xxh3-128 hashes will not produce at this scale, by forcing every page onto a
// single namespace key. All three pages must survive the round trip, each
// found under its own 128-bit hash — the `full_hash` disambiguation CAS spec
// §5.4 makes mandatory.
func TestATruncatedKeyCollisionIsResolvedByTheFullHash(t *testing.T) {
	collide := func(xxh3.Uint128) uint64 { return 0x5ca1ab1e }

	c := NewPerTraceCache()
	for i := int64(1); i <= 3; i++ {
		p := page(i)
		if err := c.Insert(HashPage(p), p); err != nil {
			t.Fatal(err)
		}
	}
	raw := encodePageStoreKeyed(c, collide)

	// One namespace key, three CAS entries chained under it.
	tree, err := nimLoadCowBTree(raw, nimCltTypeB)
	if err != nil {
		t.Fatalf("the producer's reader rejected the stream: %v", err)
	}
	desc, err := tree.nimLookup(0x5ca1ab1e)
	if err != nil {
		t.Fatalf("the colliding key was not found: %v", err)
	}
	if got := binary.LittleEndian.Uint64(desc[8:]); got != uint64(3*(pageRecordHeaderSize+PageSize)) {
		t.Errorf("the colliding key's payload is %d bytes, want three CAS entries", got)
	}
	if _, err := tree.nimLookup(0x5ca1ab1f); err == nil {
		t.Error("a key that was never inserted resolved")
	}

	back, err := decodePageStoreKeyed(raw, collide)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Len() != c.Len() {
		t.Fatalf("a truncated-key collision cost %d of %d page(s)",
			c.Len()-back.Len(), c.Len())
	}
	for h, want := range c.pages {
		got, ok := back.Lookup(h)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("page %x did not survive a truncated-key collision", h.Bytes())
		}
	}
}

// TestAnEmptyPageStoreIsAValidEmptyNamespace: a recording that introduced no
// new page still writes a stream, and it must be a namespace a reader accepts
// rather than a special case.
func TestAnEmptyPageStoreIsAValidEmptyNamespace(t *testing.T) {
	raw := encodePageStore(NewPerTraceCache())
	if len(raw) != nsPageSize {
		t.Errorf("an empty page store is %d bytes, want one %d-byte header page",
			len(raw), nsPageSize)
	}
	tree, err := nimLoadCowBTree(raw, nimCltTypeB)
	if err != nil {
		t.Fatalf("the producer's reader rejected an empty namespace: %v", err)
	}
	if tree.nimCommittedRoot() != 0 {
		t.Errorf("an empty namespace published root %d", tree.nimCommittedRoot())
	}
	if _, err := tree.nimLookup(1); err == nil {
		t.Error("an empty namespace resolved a key")
	}
	back, err := decodePageStore(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Len() != 0 {
		t.Errorf("an empty page store decoded %d page(s)", back.Len())
	}
}
