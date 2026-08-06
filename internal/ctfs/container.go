// Package ctfs is a minimal, self-contained **reader** for the CTFS container
// format — the single-file `.ct` bundle CodeTracer traces are stored in.
//
// The format is specified by
// `codetracer-trace-format-spec/ctfs-container.md` and (identically)
// `codetracer-specs/Trace-Files/CTFS-Binary-Format.md`. This package
// implements the subset the WASM replay snapshotter needs:
//
//   - open an existing container and enumerate its internal files;
//   - read an internal file's raw bytes.
//
// # It used to write, and that is exactly why it no longer does
//
// `WASM-Replay-Snapshots-And-Slices.md` §6 is emphatic that snapshots are
// "**not** sidecar files. They live inside the trace container" — but the
// `.ct` is produced by `codetracer-trace-format-nim`'s CTFS writer through
// the C FFI in `tracewriter/`, and that container is already **closed** by
// the time snapshots are attached. With no "add an internal file to a closed
// container" entry point in the FFI, this package grew an appender: enough of
// the on-disk layout re-implemented to write into a sealed container.
//
// It drifted. The two writers disagreed about whether the multi-level block
// mapping of §4 is cumulative; containers written here were silently mis-read
// by `ct-print`, `ct-space` and the db-backend past ~511 data blocks (~2 MB at
// the default 4096-byte block) — 364 KB of wrong bytes, no error raised. The
// spec was ambiguous, both readings were defensible, and this package's own
// round-trip test could not see it because it wrote and read with the same
// convention.
//
// M38b closed the gap at the source: `ct_container_append_files` in the
// canonical writer, bound by `internal/ctfsffi`. The writer half of this
// package is gone with it.
//
// # Why the reader stays
//
// Because a round trip through one implementation proves nothing, which is
// the whole lesson above. An FFI-written container is adjudicated by a reader
// that shares no code with the writer — a different implementation in a
// different language — and this is that reader. Deleting it would leave the
// canonical writer's newest and least-exercised path checked only by the
// implementation that produced the bytes.
//
// It is pure Go and depends on no toolchain, which is what lets
// `multilevel_layout_test.go` pin it against a committed Nim-written fixture
// with nothing installed.
//
// # Deliberate limits
//
//   - **Read-only.** Nothing here mutates a container. Use `internal/ctfsffi`.
//   - **No namespace B-tree in this package.** The `NSB1` namespace machinery
//     of `CTFS-Binary-Format.md` §10 is not implemented here: this package
//     stores and retrieves whole internal files, and indexes nothing by key.
//     That is a scope choice rather than a gap in the specification — §10's
//     "Key Lookup: B-Tree Index" now gives the interior-node layout as well as
//     the 61-byte header and the leaf descriptors, and
//     `internal/wasmsnapshot`'s `nsb1.go` writes and reads a real one for the
//     `snappages.ns` per-trace page store. That namespace travels through this
//     package as opaque bytes, exactly like every other stream.
//   - **No compression.** The container layer is compression-agnostic
//     (`ctfs-container.md` §1: "Compression is not in the header"); the
//     streams read here are stored raw and self-describe as such.
package ctfs

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
)

// Container-header field layout (`ctfs-container.md` §1).
const (
	headerSize     = 16
	fileEntrySize  = 24
	magicLen       = 5
	offsetVersion  = 5
	offsetEncrypt  = 6
	offsetShards   = 7
	offsetBlockSz  = 8
	offsetMaxRootE = 12
)

// magic is `C0 DE 72 AC E2` — "CODE TRACE".
var magic = [magicLen]byte{0xc0, 0xde, 0x72, 0xac, 0xe2}

// FileEntry is one 24-byte slot of the block-0 entry array.
type FileEntry struct {
	Size     uint64
	MapBlock uint64
	Name     uint64
	// Slot is the entry's index in the block-0 array. Nothing here writes an
	// entry back, but a diagnostic that says *which* slot is malformed is the
	// difference between a usable error and "the container is broken".
	Slot int
}

// Container is an opened `.ct` file.
type Container struct {
	path       string
	blockSize  uint64
	maxShards  uint8
	version    uint8
	encryption uint8
	// entryBase is the byte offset of the first FileEntry in block 0.
	entryBase int
	// entryCount is the number of FileEntry slots.
	entryCount int
	// block0 is the whole of block 0, held in memory and rewritten on
	// commit. Block 0 is the only block this package mutates.
	block0 []byte
	// nextFreeBlock is the bump allocator. `ctfs-container.md` §4 keeps it
	// as live shared state during recording and does not persist it, so for
	// a quiescent container it is recovered from the file length — every
	// allocated block is materialised on disk, and the file is a whole
	// number of blocks.
	nextFreeBlock uint64
	fileSize      uint64
}

// Open reads a container's structure. The file is not held open.
func Open(path string) (*Container, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ctfs: opening container: %w", err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("ctfs: stat %s: %w", path, err)
	}
	if st.Size() < headerSize {
		return nil, fmt.Errorf("ctfs: %s is %d bytes, too small to be a container", path, st.Size())
	}

	var hdr [headerSize]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return nil, fmt.Errorf("ctfs: reading %s container header: %w", path, err)
	}
	for i := 0; i < magicLen; i++ {
		if hdr[i] != magic[i] {
			return nil, fmt.Errorf(
				"ctfs: %s does not start with the CTFS magic C0DE72ACE2; it is not a "+
					"CTFS container", path)
		}
	}

	c := &Container{
		path:       path,
		version:    hdr[offsetVersion],
		encryption: hdr[offsetEncrypt],
		maxShards:  hdr[offsetShards],
		blockSize:  uint64(binary.LittleEndian.Uint32(hdr[offsetBlockSz:])),
		fileSize:   uint64(st.Size()),
	}
	if c.encryption != 0 {
		return nil, fmt.Errorf(
			"ctfs: %s is encrypted (encryption=%d); its contents are opaque without "+
				"the key, so no internal file can be read or added", path, c.encryption)
	}
	if c.blockSize == 0 || c.blockSize%8 != 0 || c.blockSize < headerSize {
		return nil, fmt.Errorf("ctfs: %s declares an unusable block size of %d", path, c.blockSize)
	}
	if c.fileSize%c.blockSize != 0 {
		return nil, fmt.Errorf(
			"ctfs: %s is %d bytes, not a whole number of %d-byte blocks; it is "+
				"truncated or still being written", path, c.fileSize, c.blockSize)
	}
	c.nextFreeBlock = c.fileSize / c.blockSize

	c.block0 = make([]byte, c.blockSize)
	if _, err := f.ReadAt(c.block0, 0); err != nil {
		return nil, fmt.Errorf("ctfs: reading %s block 0: %w", path, err)
	}

	maxRootEntries := int(binary.LittleEndian.Uint32(hdr[offsetMaxRootE:]))
	if err := c.locateEntryArray(maxRootEntries); err != nil {
		return nil, err
	}
	return c, nil
}

// locateEntryArray finds the byte offset of the FileEntry array in block 0.
//
// `ctfs-container.md` §1 places a free-list root area of
// `R = 8 * MaxShards * 6` bytes between the header and the entry array — one
// root per shard per size class, seven sub-block classes plus the whole-block
// class, six bytes each. Containers written by the Nim writer for an unsharded
// trace carry `MaxShards = 1` yet start their entry array immediately after
// the 16-byte header, i.e. with `R = 0`: its `fileEntryOffset` is
// unconditionally `HeaderSize + ExtHeaderSize + index * FileEntrySize`, and
// the per-shard free lists only exist once there is sub-block allocation to
// track.
//
// Rather than hard-code either reading, both candidate offsets are validated
// structurally and the first that holds up is used. Validation is strict: it
// is not a heuristic that lets a wrong guess through, because every non-empty
// entry must round-trip through base40 (the encoding is not surjective onto
// u64, so random bytes essentially never do) and must point inside the file.
func (c *Container) locateEntryArray(maxRootEntries int) error {
	candidates := []int{headerSize}
	if r := 8 * int(c.maxShards) * 6; r > 0 {
		if c.maxShards <= 1 {
			candidates = append(candidates, headerSize+r)
		} else {
			candidates = []int{headerSize + r, headerSize}
		}
	}
	var reasons []string
	for _, base := range candidates {
		count := maxRootEntries
		if count == 0 {
			count = (int(c.blockSize) - base) / fileEntrySize
		}
		if base+count*fileEntrySize > int(c.blockSize) {
			// Entry arrays may overflow into blocks after block 0. Nothing
			// this package writes does, and supporting it without a sample
			// to test against would be untested code, so it is refused.
			reasons = append(reasons, fmt.Sprintf(
				"at offset %d the %d declared entries would overflow block 0", base, count))
			continue
		}
		c.entryBase, c.entryCount = base, count
		if err := c.validateEntries(); err != nil {
			reasons = append(reasons, fmt.Sprintf("at offset %d: %v", base, err))
			continue
		}
		return nil
	}
	c.entryBase, c.entryCount = 0, 0
	return fmt.Errorf(
		"ctfs: cannot locate the file-entry array in %s block 0 (%v)", c.path, reasons)
}

func (c *Container) validateEntries() error {
	nonEmpty := 0
	for i := 0; i < c.entryCount; i++ {
		e := c.entryAt(i)
		if e.Size == 0 && e.MapBlock == 0 && e.Name == 0 {
			continue
		}
		nonEmpty++
		name := DecodeName(e.Name)
		if name == "" {
			return fmt.Errorf("entry %d has a non-zero name that decodes to nothing", i)
		}
		back, err := EncodeName(name)
		if err != nil || back != e.Name {
			return fmt.Errorf("entry %d's name %#x is not a valid base40 encoding", i, e.Name)
		}
		if e.MapBlock >= c.nextFreeBlock {
			return fmt.Errorf(
				"entry %d (%q) points at block %d but the container only has %d blocks",
				i, name, e.MapBlock, c.nextFreeBlock)
		}
	}
	if nonEmpty == 0 {
		return fmt.Errorf("no non-empty entries")
	}
	return nil
}

func (c *Container) entryAt(i int) FileEntry {
	off := c.entryBase + i*fileEntrySize
	return FileEntry{
		Size:     binary.LittleEndian.Uint64(c.block0[off:]),
		MapBlock: binary.LittleEndian.Uint64(c.block0[off+8:]),
		Name:     binary.LittleEndian.Uint64(c.block0[off+16:]),
		Slot:     i,
	}
}

// BlockSize reports the container's block size in bytes.
func (c *Container) BlockSize() uint64 { return c.blockSize }

// Names lists the internal files present, sorted.
func (c *Container) Names() []string {
	var out []string
	for i := 0; i < c.entryCount; i++ {
		e := c.entryAt(i)
		if e.Name == 0 {
			continue
		}
		out = append(out, DecodeName(e.Name))
	}
	sort.Strings(out)
	return out
}

// Has reports whether an internal file of this name exists.
func (c *Container) Has(name string) bool {
	_, err := c.find(name)
	return err == nil
}

func (c *Container) find(name string) (FileEntry, error) {
	want, err := EncodeName(name)
	if err != nil {
		return FileEntry{}, err
	}
	for i := 0; i < c.entryCount; i++ {
		e := c.entryAt(i)
		if e.Name == want {
			return e, nil
		}
	}
	return FileEntry{}, fmt.Errorf("ctfs: %s contains no internal file named %q", c.path, name)
}

// usable is the number of child pointers per mapping block; the last u64 of
// each level-head mapping block is the chain pointer to the next level
// (`ctfs-container.md` §4).
func (c *Container) usable() uint64 { return c.blockSize/8 - 1 }

// ReadFile returns an internal file's bytes.
func (c *Container) ReadFile(name string) ([]byte, error) {
	e, err := c.find(name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(c.path)
	if err != nil {
		return nil, fmt.Errorf("ctfs: opening container: %w", err)
	}
	defer f.Close()
	return c.readEntry(f, e)
}

func (c *Container) readEntry(f *os.File, e FileEntry) ([]byte, error) {
	out := make([]byte, e.Size)
	if e.Size == 0 {
		return out, nil
	}
	blocks, err := c.dataBlocks(f, e)
	if err != nil {
		return nil, err
	}
	for i, blk := range blocks {
		start := uint64(i) * c.blockSize
		end := start + c.blockSize
		if end > e.Size {
			end = e.Size
		}
		if _, err := f.ReadAt(out[start:end], int64(blk*c.blockSize)); err != nil {
			return nil, fmt.Errorf(
				"ctfs: reading %q (entry slot %d) block %d (container block %d): %w",
				DecodeName(e.Name), e.Slot, i, blk, err)
		}
	}
	return out, nil
}

// maxMapLevels is the container spec's five-level cap on the mapping
// hierarchy (`ctfs-container.md` §4; `MaxChainLevels` in the Nim writer).
const maxMapLevels = 5

// levelCapacity is the number of data blocks a *single* mapping block at
// `level` addresses: `usable` at level 1, `usable^2` at level 2, and so on.
func levelCapacity(usable uint64, level int) uint64 {
	capacity := uint64(1)
	for i := 0; i < level; i++ {
		capacity *= usable
	}
	return capacity
}

// resolveDataBlock maps a file-relative block index to a container block
// number, following the producer's bottom-up chain model exactly.
//
// # Why the levels are *cumulative*, and why that is not a free choice
//
// `ctfs-container.md` §4's diagram once admitted two readings: either the
// level-2 block re-parents the level-1 root (so its slot 0 covers data blocks
// 0..usable-1 again), or the level-2 block covers only the blocks *past* what
// the root already holds. The two produce different bytes for any file larger
// than `usable` data blocks — about 2 MiB at the default 4096-byte block size
// — and a reader using the wrong one silently returns the wrong data blocks
// rather than failing. `CTFS-Binary-Format.md` §4 now settles it normatively,
// in favour of the second reading; this comment records why it had to.
//
// The producer settles it. `codetracer-trace-format-nim`'s
// `block_mapping.nim` (`insertDataBlock` / `lookupDataBlock`, mirroring the
// Rust `CtfsWriter`) walks *up* subtracting each level's capacity —
//
//	idx = blockIndex; level = 1; cur = MapBlock
//	while idx >= usable^level: idx -= usable^level; level++; cur = cur[usable]
//
// — so the level-1 root holds data blocks `[0, usable)`, the level-2 block
// holds `[usable, usable + usable^2)` with the index rebased, and so on. This
// package transcribes that walk independently — same algorithm, no shared
// code — which is what makes it usable as the adjudicator of an FFI-written
// container in `ffi_crossread_test.go`. The alternative reading is not merely
// a different-but-valid layout: a container written that way is *mis-read* by
// `ct-print`, `ct-space` and the db-backend, which is the wrong-bytes failure
// the snapshot design must not have.
//
// # The small-file optimisation is deliberately not implemented
//
// §2 of the container spec describes a small-file optimisation: when
// `Size <= BlockSize`, `MapBlock` "points directly to the single data block"
// rather than to a mapping block. The CTFS writer that actually produces
// CodeTracer traces does **not** apply it: `addFile` in
// `codetracer-trace-format-nim`'s `container.nim` allocates a level-1 mapping
// block unconditionally, before it knows how large the file will be, so even a
// 3-byte internal file gets one.
//
// The two conventions cannot be told apart from the bytes. A 4-byte file whose
// content happens to be a small little-endian integer is indistinguishable
// from a mapping block pointing at that block number, and `events.idx` in a
// real trace is exactly such a file. Guessing would therefore mean occasionally
// returning a block number where a reader asked for content.
//
// So this package follows the producer, not the prose: it always resolves
// through a mapping block.
func (c *Container) resolveDataBlock(f *os.File, e FileEntry, blockIdx uint64,
	block func(uint64) ([]byte, error),
) (uint64, error) {
	usable := c.usable()
	name := DecodeName(e.Name)

	// Ascend the chain until a level whose capacity covers what is left.
	idx, level, cur := blockIdx, 1, e.MapBlock
	for {
		capacity := levelCapacity(usable, level)
		if idx < capacity {
			break
		}
		idx -= capacity
		level++
		if level > maxMapLevels {
			return 0, fmt.Errorf(
				"ctfs: %q block index %d is beyond the %d-level mapping limit",
				name, blockIdx, maxMapLevels)
		}
		blk, err := block(cur)
		if err != nil {
			return 0, err
		}
		chain := binary.LittleEndian.Uint64(blk[usable*8:])
		if chain == 0 {
			return 0, fmt.Errorf(
				"ctfs: %q needs a level-%d mapping block for block index %d but its "+
					"level-%d block has no chain pointer", name, level, blockIdx, level-1)
		}
		cur = chain
	}

	// Descend to the level-1 block holding the pointer.
	for level > 1 {
		subCap := levelCapacity(usable, level-1)
		entryIdx, subIdx := idx/subCap, idx%subCap
		blk, err := block(cur)
		if err != nil {
			return 0, err
		}
		child := binary.LittleEndian.Uint64(blk[entryIdx*8:])
		if child == 0 {
			return 0, fmt.Errorf(
				"ctfs: %q has a hole in its level-%d mapping block at slot %d",
				name, level, entryIdx)
		}
		cur, idx, level = child, subIdx, level-1
	}

	blk, err := block(cur)
	if err != nil {
		return 0, err
	}
	dataBlk := binary.LittleEndian.Uint64(blk[idx*8:])
	if dataBlk == 0 {
		return 0, fmt.Errorf("ctfs: %q has a null data block at index %d", name, blockIdx)
	}
	return dataBlk, nil
}

// dataBlocks resolves an entry's whole data-block list. See resolveDataBlock
// for the layout it follows and why.
func (c *Container) dataBlocks(f *os.File, e FileEntry) ([]uint64, error) {
	n := (e.Size + c.blockSize - 1) / c.blockSize
	// Mapping blocks are re-visited once per data block, so cache them; a
	// 2 GiB file otherwise re-reads its level-2 spine 500k times.
	cache := map[uint64][]byte{}
	block := func(blk uint64) ([]byte, error) {
		if b, ok := cache[blk]; ok {
			return b, nil
		}
		b, err := c.readBlock(f, blk)
		if err != nil {
			return nil, err
		}
		cache[blk] = b
		return b, nil
	}

	out := make([]uint64, 0, n)
	for i := uint64(0); i < n; i++ {
		blk, err := c.resolveDataBlock(f, e, i, block)
		if err != nil {
			return nil, err
		}
		out = append(out, blk)
	}
	return out, nil
}

func (c *Container) readBlock(f *os.File, blk uint64) ([]byte, error) {
	if blk == 0 || blk >= c.nextFreeBlock {
		return nil, fmt.Errorf(
			"ctfs: %s refers to block %d, outside the container's %d blocks",
			c.path, blk, c.nextFreeBlock)
	}
	buf := make([]byte, c.blockSize)
	if _, err := f.ReadAt(buf, int64(blk*c.blockSize)); err != nil {
		return nil, fmt.Errorf("ctfs: reading block %d of %s: %w", blk, c.path, err)
	}
	return buf, nil
}
