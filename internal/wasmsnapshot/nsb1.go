package wasmsnapshot

import (
	"encoding/binary"
	"fmt"
)

// ---------------------------------------------------------------------------
// `NSB1` — the CTFS namespace B-tree
// ---------------------------------------------------------------------------
//
// This file is a complete writer and a complete reader for the namespace
// B-tree page image specified by
// `codetracer-specs/Trace-Files/CTFS-Binary-Format.md` §10 ("Namespaces"),
// subsection **"Key Lookup: B-Tree Index"**. That subsection is normative for
// everything below — the 61-byte `NamespaceHeader`, the 8-byte node header,
// the two payload arrays, the copy-up separator rule, the fanout formula, the
// whole-page free chain, and the one-pass bulk-load packing. Nothing here is
// reverse-engineered from a producer; the spec is the source and the
// cross-language proofs in `nsb1_crossread_test.go` are what hold the two
// implementations to it.
//
// The image is a flat sequence of 4096-byte pages. Page `p` starts at byte
// `p * 4096`. Page 0 carries the `NamespaceHeader` and is never a node, so a
// child pointer is always `>= 1` and the value 0 means "none".
//
// Only the parts a bulk-loading writer and a read-only reader need are
// implemented: there is no copy-on-write insert path, no double-buffered
// commit sequence and no reader-gated reclamation, because this package
// produces a whole namespace once, from a finished set of pages, and never
// mutates it afterwards. A tree written here therefore always publishes its
// root in slot 0 with `commit_id = 1` and leaves the free chain empty — which
// §10 explicitly blesses ("publish it as the root with `commit_id = 1` in root
// slot 0"), and which a reader cannot distinguish from an incrementally built
// tree beyond the page packing.

const (
	// nsPageSize is the namespace page size. §10 fixes it at 4096 independently
	// of the container's own block size, which is why this is not
	// `ctfs.BlockSize`.
	nsPageSize = 4096

	// nsNodeHeaderBytes is `node_kind` + `reserved` + `count` + `reserved[4]`.
	nsNodeHeaderBytes = 8

	// nsHeaderTotal is the size of the `NamespaceHeader` at the start of page 0.
	nsHeaderTotal = 61

	nsKindInternal = 0
	nsKindLeaf     = 1

	// nsDescSizeTypeA / nsDescSizeTypeB are §10's two entry-descriptor widths.
	// Only Type B is written here; Type A is named so the fanout formula reads
	// the way the spec states it.
	nsDescSizeTypeA = 8
	nsDescSizeTypeB = 16

	// Header field offsets, all little-endian (§10 "Namespace header").
	nsOffRoot0     = 4
	nsOffRoot1     = 12
	nsOffCommit0   = 20
	nsOffCommit1   = 28
	nsOffFlags     = 36
	nsOffFreeHead  = 37
	nsOffNextFree  = 45
	nsOffPageCount = 53

	// Header flag bits (§10 "Namespace header").
	nsFlagLeafTypeB     = 0b01
	nsFlagSkipSubBlocks = 0b10

	// nsMaxDepth bounds a reader's descent. §10 states no depth limit — it does
	// not need one, since a well-formed tree of `order`-way nodes is shallow —
	// so this is purely a corrupt-image guard.
	nsMaxDepth = 16
)

// nsMagic is the `NSB1` identity of a copy-on-write namespace page image. It is
// the only version field the format has; see `pageStoreSidecar*` in
// `pagecas.go` for how this package carries its own stream version alongside
// it without touching the namespace wire format.
var nsMagic = [4]byte{'N', 'S', 'B', '1'}

// nsOrder is §10's fanout: `order = (4096 - 8) / (8 + D)`, i.e. 255 keys per
// node for Leaf Type A and 170 for Leaf Type B. A leaf holds at most `order`
// keys; an internal node holds at most `order` keys and `order + 1` children.
func nsOrder(descSize int) int { return (nsPageSize - nsNodeHeaderBytes) / (8 + descSize) }

// nsEntry is one `(key, entry descriptor)` pair of a namespace.
type nsEntry struct {
	key  uint64
	desc []byte
}

// ---------------------------------------------------------------------------
// Writer — the §10 one-pass bulk load
// ---------------------------------------------------------------------------

// nsBuilder accumulates namespace pages. Pages are only ever appended, because
// a bulk load allocates each node exactly once and never supersedes one.
type nsBuilder struct {
	image    []byte
	next     uint64 // bump-allocation cursor; page 0 is the header
	descSize int
}

func (b *nsBuilder) alloc() uint64 {
	p := b.next
	b.next++
	b.image = append(b.image, make([]byte, nsPageSize)...)
	return p
}

func (b *nsBuilder) base(page uint64) int { return int(page) * nsPageSize }

// writeNodeHeader lays down `node_kind [0]`, `reserved [1]`, `count [2..4)`
// and `reserved [4..8)`.
func (b *nsBuilder) writeNodeHeader(page uint64, leaf bool, count int) {
	base := b.base(page)
	if leaf {
		b.image[base] = nsKindLeaf
	} else {
		b.image[base] = nsKindInternal
	}
	b.image[base+1] = 0
	binary.LittleEndian.PutUint16(b.image[base+2:], uint16(count))
	for i := 4; i < nsNodeHeaderBytes; i++ {
		b.image[base+i] = 0
	}
}

// writeLeaf writes `count` keys at byte 8 followed by `count` D-byte
// descriptors at `8 + count*8`, with no padding between the two arrays.
func (b *nsBuilder) writeLeaf(page uint64, entries []nsEntry) {
	b.writeNodeHeader(page, true, len(entries))
	base := b.base(page)
	descBase := base + nsNodeHeaderBytes + len(entries)*8
	for i, e := range entries {
		binary.LittleEndian.PutUint64(b.image[base+nsNodeHeaderBytes+i*8:], e.key)
		copy(b.image[descBase+i*b.descSize:descBase+(i+1)*b.descSize], e.desc)
	}
}

// writeInternal writes `count` keys at byte 8 followed by `count + 1` absolute
// page numbers at `8 + count*8`. Child pointers are u64 regardless of D:
// internal nodes carry no descriptors.
func (b *nsBuilder) writeInternal(page uint64, keys []uint64, children []uint64) {
	b.writeNodeHeader(page, false, len(keys))
	base := b.base(page)
	childBase := base + nsNodeHeaderBytes + len(keys)*8
	for i, k := range keys {
		binary.LittleEndian.PutUint64(b.image[base+nsNodeHeaderBytes+i*8:], k)
	}
	for i, c := range children {
		binary.LittleEndian.PutUint64(b.image[childBase+i*8:], c)
	}
}

// nsBulkLoad packs a sorted, duplicate-free batch into a namespace image, the
// §10 "Building a tree in one pass (bulk load)" recipe:
//
//	cut the sorted entries into consecutive runs of at most `order` keys, one
//	leaf page each; then build each internal level over the level below by
//	grouping up to `order + 1` children per node and using each child's
//	subtree minimum as the separator that precedes it; repeat until one node
//	remains, and publish it as the root with `commit_id = 1` in root slot 0.
//
// The separator rule is the load-bearing half: §10's lookup descends into
// child `i + 1` when `keys[i] == key`, which is only correct because `keys[i]`
// is the *smallest* key reachable through child `i + 1` rather than an
// exclusive upper bound on child `i`.
//
// An empty batch produces a valid, empty namespace: one page, both root slots
// zero, both commit ids zero.
func nsBulkLoad(entries []nsEntry, descSize int, flags byte) ([]byte, error) {
	order := nsOrder(descSize)
	for i, e := range entries {
		if len(e.desc) != descSize {
			return nil, fmt.Errorf(
				"wasmsnapshot: namespace entry %d has a %d-byte descriptor, but the "+
					"namespace's leaf type fixes it at %d", i, len(e.desc), descSize)
		}
		if i > 0 && e.key <= entries[i-1].key {
			return nil, fmt.Errorf(
				"wasmsnapshot: namespace batch is not strictly ascending at entry %d "+
					"(key %#x follows %#x); §10 requires the keys within a node to be "+
					"sorted and unique", i, e.key, entries[i-1].key)
		}
	}

	b := &nsBuilder{
		image:    make([]byte, nsPageSize), // page 0 — the NamespaceHeader
		next:     1,
		descSize: descSize,
	}

	// levelNode is a node of the level currently being packed, plus the
	// smallest key reachable through it — the separator material for the level
	// above.
	type levelNode struct {
		page   uint64
		minKey uint64
	}

	var level []levelNode
	for i := 0; i < len(entries); {
		run := order
		if n := len(entries) - i; n < run {
			run = n
		}
		page := b.alloc()
		b.writeLeaf(page, entries[i:i+run])
		level = append(level, levelNode{page: page, minKey: entries[i].key})
		i += run
	}

	for len(level) > 1 {
		var parent []levelNode
		for c := 0; c < len(level); {
			group := order + 1
			if n := len(level) - c; n < group {
				group = n
			}
			keys := make([]uint64, 0, group-1)
			children := make([]uint64, group)
			for g := 0; g < group; g++ {
				children[g] = level[c+g].page
				if g > 0 {
					keys = append(keys, level[c+g].minKey)
				}
			}
			page := b.alloc()
			b.writeInternal(page, keys, children)
			parent = append(parent, levelNode{page: page, minKey: level[c].minKey})
			c += group
		}
		level = parent
	}

	var root, commit uint64
	if len(level) == 1 {
		root, commit = level[0].page, 1
	}

	copy(b.image[0:4], nsMagic[:])
	binary.LittleEndian.PutUint64(b.image[nsOffRoot0:], root)
	binary.LittleEndian.PutUint64(b.image[nsOffRoot1:], 0)
	binary.LittleEndian.PutUint64(b.image[nsOffCommit0:], commit)
	binary.LittleEndian.PutUint64(b.image[nsOffCommit1:], 0)
	b.image[nsOffFlags] = flags
	binary.LittleEndian.PutUint64(b.image[nsOffFreeHead:], 0)
	binary.LittleEndian.PutUint64(b.image[nsOffNextFree:], b.next)
	binary.LittleEndian.PutUint64(b.image[nsOffPageCount:], b.next)
	return b.image, nil
}

// ---------------------------------------------------------------------------
// Reader
// ---------------------------------------------------------------------------

// nsHeader is the decoded `NamespaceHeader` of page 0.
type nsHeader struct {
	root      uint64
	commitID  uint64
	flags     byte
	pageCount uint64
}

// nsReadHeader validates and decodes page 0.
//
// The committed root is the slot with the higher `commit_id`; `commit_id == 0`
// means the slot is empty, and both zero means the namespace is empty (§10).
func nsReadHeader(image []byte) (nsHeader, error) {
	var h nsHeader
	if len(image) < nsHeaderTotal {
		return h, fmt.Errorf(
			"wasmsnapshot: namespace image is %d bytes, too short for its %d-byte "+
				"NSB1 header", len(image), nsHeaderTotal)
	}
	if [4]byte(image[0:4]) != nsMagic {
		return h, fmt.Errorf(
			"wasmsnapshot: namespace image does not carry the %q magic",
			string(nsMagic[:]))
	}
	if len(image)%nsPageSize != 0 {
		return h, fmt.Errorf(
			"wasmsnapshot: namespace image is %d bytes, not a whole number of "+
				"%d-byte pages", len(image), nsPageSize)
	}
	commit0 := binary.LittleEndian.Uint64(image[nsOffCommit0:])
	commit1 := binary.LittleEndian.Uint64(image[nsOffCommit1:])
	switch {
	case commit0 == 0 && commit1 == 0:
		h.root, h.commitID = 0, 0
	case commit1 > commit0:
		h.root, h.commitID = binary.LittleEndian.Uint64(image[nsOffRoot1:]), commit1
	default:
		h.root, h.commitID = binary.LittleEndian.Uint64(image[nsOffRoot0:]), commit0
	}
	h.flags = image[nsOffFlags]
	h.pageCount = binary.LittleEndian.Uint64(image[nsOffPageCount:])
	if h.pageCount > uint64(len(image)/nsPageSize) {
		return h, fmt.Errorf(
			"wasmsnapshot: the NSB1 header declares %d page(s) but the image holds %d",
			h.pageCount, len(image)/nsPageSize)
	}
	return h, nil
}

// nsCollect walks the committed tree and returns every `(key, descriptor)` in
// ascending key order.
//
// It is a *validating* walk: a corrupt image must produce a diagnostic, never a
// panic and never a silently short result. Every structural rule §10 states is
// checked — page number in range, node kind known, `count <= order`, both
// payload arrays inside the page, keys ascending within a node and across the
// whole traversal — and a page is refused if it is reached twice, which is what
// bounds the walk on an image whose child pointers form a cycle.
func nsCollect(image []byte, descSize int) ([]nsEntry, error) {
	h, err := nsReadHeader(image)
	if err != nil {
		return nil, err
	}
	if h.root == 0 {
		return nil, nil
	}
	order := nsOrder(descSize)
	seen := map[uint64]bool{}

	// node is a page plus its decoded header, validated on the way in.
	type node struct {
		page  uint64
		leaf  bool
		count int
		base  int
	}
	read := func(page uint64) (node, error) {
		var n node
		if page == 0 {
			return n, fmt.Errorf(
				"wasmsnapshot: namespace child pointer 0 was followed; page 0 is the " +
					"NamespaceHeader and is never a node")
		}
		if page >= h.pageCount {
			return n, fmt.Errorf(
				"wasmsnapshot: namespace child pointer names page %d but the header "+
					"declares only %d page(s)", page, h.pageCount)
		}
		if seen[page] {
			return n, fmt.Errorf(
				"wasmsnapshot: namespace page %d is reachable twice; the page graph "+
					"is not a tree", page)
		}
		seen[page] = true
		n.page = page
		n.base = int(page) * nsPageSize
		switch image[n.base] {
		case nsKindLeaf:
			n.leaf = true
		case nsKindInternal:
			n.leaf = false
		default:
			return n, fmt.Errorf(
				"wasmsnapshot: namespace page %d has node kind %d; §10 defines only "+
					"0 (internal) and 1 (leaf)", page, image[n.base])
		}
		n.count = int(binary.LittleEndian.Uint16(image[n.base+2:]))
		if n.count > order {
			return n, fmt.Errorf(
				"wasmsnapshot: namespace page %d holds %d keys, above the fanout of "+
					"%d that a %d-byte page and a %d-byte descriptor allow",
				page, n.count, order, nsPageSize, descSize)
		}
		// Both payload arrays must lie inside the page. With `count <= order`
		// this cannot fail, so it is a belt-and-braces check against a future
		// change to the fanout formula rather than against a hostile image.
		width := descSize
		extra := 0
		if !n.leaf {
			width, extra = 8, 8 // count+1 children
		}
		if nsNodeHeaderBytes+n.count*8+n.count*width+extra > nsPageSize {
			return n, fmt.Errorf(
				"wasmsnapshot: namespace page %d declares %d keys, whose payload does "+
					"not fit in a %d-byte page", page, n.count, nsPageSize)
		}
		return n, nil
	}
	key := func(n node, i int) uint64 {
		return binary.LittleEndian.Uint64(image[n.base+nsNodeHeaderBytes+i*8:])
	}

	var out []nsEntry
	var walk func(page uint64, depth int) error
	walk = func(page uint64, depth int) error {
		// A fanout of 170 reaches 170^8 > 6.7e17 keys in eight levels, so a
		// deeper tree than this is a corrupt image rather than a large one. The
		// bound is what keeps the walk off the Go stack's limits on an image
		// whose child pointers chain rather than branch.
		if depth > nsMaxDepth {
			return fmt.Errorf(
				"wasmsnapshot: namespace tree is deeper than %d levels; the page graph "+
					"is corrupt", nsMaxDepth)
		}
		n, err := read(page)
		if err != nil {
			return err
		}
		for i := 1; i < n.count; i++ {
			if key(n, i) <= key(n, i-1) {
				return fmt.Errorf(
					"wasmsnapshot: namespace page %d has keys %#x then %#x; §10 requires "+
						"them sorted ascending and unique", page, key(n, i-1), key(n, i))
			}
		}
		if n.leaf {
			descBase := n.base + nsNodeHeaderBytes + n.count*8
			for i := 0; i < n.count; i++ {
				k := key(n, i)
				if len(out) > 0 && k <= out[len(out)-1].key {
					return fmt.Errorf(
						"wasmsnapshot: namespace key %#x is not greater than the preceding "+
							"key %#x; the leaves are out of order", k, out[len(out)-1].key)
				}
				d := make([]byte, descSize)
				copy(d, image[descBase+i*descSize:descBase+(i+1)*descSize])
				out = append(out, nsEntry{key: k, desc: d})
			}
			return nil
		}
		childBase := n.base + nsNodeHeaderBytes + n.count*8
		for i := 0; i <= n.count; i++ {
			child := binary.LittleEndian.Uint64(image[childBase+i*8:])
			if err := walk(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(h.root, 1); err != nil {
		return nil, err
	}
	return out, nil
}
