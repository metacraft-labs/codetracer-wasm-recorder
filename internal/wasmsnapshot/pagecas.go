package wasmsnapshot

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tetratelabs/wazero/internal/xxh3"
)

// PageSize is the content-addressing granularity: one WebAssembly page.
//
// The CAS spec fixes the page size to the OS page size because that is what
// `cp.entry.lay` regions are made of. The snapshot spec §5 observes that for
// WASM "page size is a free parameter rather than an OS constant; 64 KiB (the
// WASM page) is the natural choice and aligns with `memory.grow`" — so a
// snapshot's region boundaries coincide exactly with the units `memory.grow`
// adds, and growing a memory can never re-align existing pages.
const PageSize = 65536

// Cache tier identifiers, per CAS spec §5. IDs 0 and 1 are reserved for the
// per-trace and system tiers; remote caches are numbered from 2 and are
// declared in the trace's metadata.
const (
	CacheIDPerTrace uint64 = 0
	CacheIDSystem   uint64 = 1
	// CacheIDFirstRemote is the lowest id a configured remote may take.
	CacheIDFirstRemote uint64 = 2
)

// Region kinds, per CAS spec §3. The values are the same three the MCR layout
// uses, so a reader of either layout dispatches identically.
const (
	// KindFull carries its bytes inline in `wcpentry.mem`.
	KindFull uint8 = 0
	// KindHashRef is a flat list of page hashes resolved through the tiers.
	KindHashRef uint8 = 1
	// KindRunHashRef is a run-length list of (tier, hashes) — the common case.
	KindRunHashRef uint8 = 2
)

// HashPage returns a page's content address. The CAS spec §4 fixes the
// algorithm at xxh3-128 and records it in the trace's metadata as
// `"cas_format": "page-xxh3-128"`; `casFormat` below carries the same string
// into `wcp.idx`.
func HashPage(page []byte) xxh3.Uint128 { return xxh3.Hash128(page) }

// PageCache is one tier of the three-tier lookup (CAS spec §5).
type PageCache interface {
	// CacheID is the tier's discriminator as written into a `kind=2` run.
	CacheID() uint64
	// Lookup returns the page bytes for a hash, if this tier holds them.
	Lookup(h xxh3.Uint128) ([]byte, bool)
	// Insert adds a page. Tiers that are read-only return an error.
	Insert(h xxh3.Uint128, page []byte) error
	// Describe names the tier for a diagnostic.
	Describe() string
}

// ---------------------------------------------------------------------------
// Tier 1 — the per-trace cache (`wcppages.ns`)
// ---------------------------------------------------------------------------

// PerTraceCache is the CAS spec §5.1 per-trace tier: "the new pages this
// recording actually introduced … the trace's self-sufficiency guarantee".
// It is serialised into the container's `wcppages.ns` stream, so a `.ct`
// carrying only `kind=2` regions plus a populated per-trace cache replays with
// a cold system cache and no network.
type PerTraceCache struct {
	// pages is keyed on the full 128-bit hash rather than the truncated
	// 64-bit namespace key, so an in-memory lookup can never suffer the
	// truncation collision §5.4 guards against on disk.
	pages map[xxh3.Uint128][]byte
}

func NewPerTraceCache() *PerTraceCache {
	return &PerTraceCache{pages: map[xxh3.Uint128][]byte{}}
}

func (c *PerTraceCache) CacheID() uint64 { return CacheIDPerTrace }

func (c *PerTraceCache) Lookup(h xxh3.Uint128) ([]byte, bool) {
	p, ok := c.pages[h]
	return p, ok
}

func (c *PerTraceCache) Insert(h xxh3.Uint128, page []byte) error {
	if _, ok := c.pages[h]; ok {
		return nil
	}
	c.pages[h] = append([]byte(nil), page...)
	return nil
}

func (c *PerTraceCache) Describe() string { return "per-trace cache (wcppages.ns)" }

// Len reports how many distinct pages the tier holds.
func (c *PerTraceCache) Len() int { return len(c.pages) }

// Bytes reports the total page *content* held, which is the dominant term in
// what the tier contributes to a container's `wcppages.ns` stream, and the
// term `SlicePolicy.TargetBytes` is measured against.
//
// It is a lower bound on the stream, not its size. `encodePageStore` writes a
// real `NSB1` namespace, so the stream also carries one 4096-byte header page,
// the B-tree's own pages (one 4096-byte leaf per 170 pages, plus the interior
// levels above them), a 24-byte CAS entry header per page, and up to 4095
// bytes of alignment padding. At a 64 KiB page that overhead is ~0.05% and it
// is deliberately not folded in here: `TargetBytes` is a knob over how much
// *memory content* a slice accumulates, and mixing index overhead into it
// would make the same policy mean different things at different key counts.
func (c *PerTraceCache) Bytes() int64 {
	var n int64
	for _, p := range c.pages {
		n += int64(len(p))
	}
	return n
}

// ---------------------------------------------------------------------------
// Tier 2 — the system cache
// ---------------------------------------------------------------------------

// SystemCache is the CAS spec §5.2 host-wide tier, rooted at `$CT_CAS_ROOT`
// (default `~/.codetracer/cas/`).
//
// The CAS spec §6 recommends a CTFS namespace for this store but leaves the
// choice open (§12 question 1) pending a benchmark that has not been run.
// This implementation uses §6's option (e), one file per hash sharded by hash
// prefix, because it is the only candidate that adds no dependency and needs
// no cross-process write protocol: a page file is created with `O_EXCL` and
// its content is its own name, so two concurrent recordings writing the same
// page cannot produce a wrong result. Its documented weaknesses (metadata
// overhead, slow GC walks) are irrelevant at the scale a WASM module's memory
// reaches and would matter only if this store were shared with the native
// recorder, which it is not — the file layout is versioned under a
// `page-xxh3-128/` directory precisely so a later switch is a new subtree
// rather than a migration.
type SystemCache struct {
	root string
}

// CASRootEnv names the environment variable that overrides the system cache
// location (CAS spec §5.2).
const CASRootEnv = "CT_CAS_ROOT"

// OpenSystemCache returns the host-wide tier, or nil when none is configured
// and none can be located. A nil *SystemCache is a valid, always-missing tier
// rather than an error: CAS spec §9 makes the system cache an optimisation
// whose absence must not stop a replay.
func OpenSystemCache() *SystemCache {
	root := os.Getenv(CASRootEnv)
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		root = filepath.Join(home, ".codetracer", "cas")
	}
	return &SystemCache{root: filepath.Join(root, "page-xxh3-128")}
}

func (c *SystemCache) CacheID() uint64 { return CacheIDSystem }

func (c *SystemCache) Describe() string {
	if c == nil {
		return "system cache (not configured)"
	}
	return fmt.Sprintf("system cache (%s)", c.root)
}

func (c *SystemCache) pathFor(h xxh3.Uint128) string {
	b := h.Bytes()
	s := hex.EncodeToString(b[:])
	return filepath.Join(c.root, s[0:2], s[2:4], s)
}

func (c *SystemCache) Lookup(h xxh3.Uint128) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	data, err := os.ReadFile(c.pathFor(h))
	if err != nil {
		return nil, false
	}
	// Re-verify: a corrupted or truncated cache file would otherwise inject
	// wrong bytes into a replay, which is worse than the slow path it
	// replaces. The check is the file-level analogue of §5.4's full-hash
	// guard against a truncated-key collision.
	if HashPage(data) != h {
		return nil, false
	}
	return data, true
}

func (c *SystemCache) Insert(h xxh3.Uint128, page []byte) error {
	if c == nil {
		return fmt.Errorf("wasmsnapshot: no system cache is configured")
	}
	p := c.pathFor(h)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			// Another recording got there first with the same content — by
			// construction, since the name IS the content address.
			return nil
		}
		return err
	}
	defer f.Close()
	if _, err := f.Write(page); err != nil {
		return err
	}
	return f.Sync()
}

// ---------------------------------------------------------------------------
// The three-tier lookup
// ---------------------------------------------------------------------------

// Tiers is the fixed per-trace → system → remote lookup order of CAS spec §5.
//
// A miss at every tier is an unrecoverable error, never a best-effort replay.
// That rule is the reason `Resolve` returns an error naming the missing hash
// instead of a zero page: a snapshot that silently materialised zeroes where a
// page should be would produce a trace describing an execution that never
// happened, which is precisely what the boundary model's divergence discipline
// exists to prevent.
type Tiers struct {
	PerTrace *PerTraceCache
	System   *SystemCache
	// Remote is unimplemented. CAS spec §5.3 configures remote endpoints in a
	// host-level config file and §8 sketches the pull protocol; neither
	// exists yet. A recording that references `cache_id >= 2` is therefore
	// rejected with a diagnostic naming the id, rather than being replayed
	// against whatever the local tiers happen to hold.
	Remote []PageCache
}

// NewTiers builds the standard tier stack: a fresh per-trace cache and,
// when one is configured, the host-wide system cache.
func NewTiers(useSystem bool) *Tiers {
	t := &Tiers{PerTrace: NewPerTraceCache()}
	if useSystem {
		t.System = OpenSystemCache()
	}
	return t
}

// Lookup walks the tiers in order and reports which one held the page.
func (t *Tiers) Lookup(h xxh3.Uint128) ([]byte, uint64, bool) {
	if t.PerTrace != nil {
		if p, ok := t.PerTrace.Lookup(h); ok {
			return p, CacheIDPerTrace, true
		}
	}
	if t.System != nil {
		if p, ok := t.System.Lookup(h); ok {
			return p, CacheIDSystem, true
		}
	}
	for _, r := range t.Remote {
		if p, ok := r.Lookup(h); ok {
			return p, r.CacheID(), true
		}
	}
	return nil, 0, false
}

// Resolve is the replay-side lookup. The `cacheID` recorded in the run is a
// hint about where the page was found at record time, not a constraint: a
// system-cache page may since have been promoted into another host's per-trace
// tier, so all tiers are consulted regardless. A miss everywhere is fatal.
func (t *Tiers) Resolve(h xxh3.Uint128, cacheID uint64) ([]byte, error) {
	if p, _, ok := t.Lookup(h); ok {
		return p, nil
	}
	b := h.Bytes()
	tiers := []string{}
	if t.PerTrace != nil {
		tiers = append(tiers, t.PerTrace.Describe())
	}
	tiers = append(tiers, t.System.Describe())
	hint := ""
	if cacheID >= CacheIDFirstRemote {
		hint = fmt.Sprintf(
			"; the snapshot says this page came from remote cache %d, and remote "+
				"cache pulls are not implemented (CAS spec §5.3/§8), so the page "+
				"cannot be fetched", cacheID)
	}
	return nil, fmt.Errorf(
		"wasmsnapshot: memory page %s is present in no cache tier (%s)%s. A miss at "+
			"every tier is unrecoverable — replaying against a page whose content is "+
			"unknown would materialise a trace of an execution that never happened",
		hex.EncodeToString(b[:]), strings.Join(tiers, ", "), hint)
}

// ---------------------------------------------------------------------------
// Region descriptors (CAS spec §3)
// ---------------------------------------------------------------------------

// Region is one `wcpentry.lay` record. The field set mirrors MCR's
// `cp.entry.lay` one-for-one so the two layouts stay legible side by side,
// including the two fields WASM has no use for: `Protect` is always zero
// because linear memory has no page permissions, and `Flags` is reserved.
type Region struct {
	Base    uint64
	Size    uint64
	Protect uint32
	Flags   uint32
	Kind    uint8

	// FileOffset is the offset into `wcpentry.mem` for KindFull.
	FileOffset uint64
	// Hashes is the flat page-hash list for KindHashRef.
	Hashes []xxh3.Uint128
	// Runs is the run list for KindRunHashRef.
	Runs []HashRun
}

// HashRun is one `(cache_id, hashes)` run of a KindRunHashRef region.
type HashRun struct {
	CacheID uint64
	Hashes  []xxh3.Uint128
}

// EncodeOptions tunes how a memory image is turned into regions.
type EncodeOptions struct {
	// InlineMissedPages selects what happens to a page no tier knows.
	//
	// When false (the default) a miss is *inserted into the per-trace cache*
	// and encoded as a `kind=2` run with `cache_id = 0`. CAS spec §5.1
	// explicitly blesses this shape — "a `.ct` file containing only `kind=2`
	// regions and a populated `cppages.ns` is replayable even with a cold
	// system cache and no remote connectivity" — and it stores each distinct
	// page exactly once.
	//
	// When true a miss is encoded as a `kind=0` region whose bytes go inline
	// into `wcpentry.mem`, per the CAS spec §3 recorder sketch. That is what a
	// producer that ships no per-trace namespace must emit, and it is the only
	// mode in which `wcpentry.mem` is non-empty. It is not the default because
	// combining it with a populated per-trace cache would store every
	// introduced page twice.
	InlineMissedPages bool
	// PromoteToSystem writes missed pages into the system cache as well
	// (CAS spec §5.3's `--cas-share-system`). Off by default: populating a
	// host-wide store is a side effect a derivation run should opt into.
	PromoteToSystem bool
}

// EncodeMemory turns a linear-memory image into region descriptors, following
// the CAS spec §3 recorder algorithm: walk pages, hash each, look it up
// through the tiers, extend the current run while the resolving tier is
// unchanged, and close the run when it changes.
//
// `inline` receives the bytes of every KindFull region in emission order; the
// caller appends them to `wcpentry.mem` and the returned regions' FileOffset
// values already account for `memBase`.
func EncodeMemory(memory []byte, tiers *Tiers, memBase uint64, opts EncodeOptions) (regions []Region, inline []byte, err error) {
	if len(memory)%PageSize != 0 {
		return nil, nil, fmt.Errorf(
			"wasmsnapshot: linear memory is %d bytes, not a whole number of %d-byte "+
				"WASM pages; a memory that is not page-aligned cannot be content-addressed "+
				"at page granularity", len(memory), PageSize)
	}
	if tiers == nil || tiers.PerTrace == nil {
		return nil, nil, fmt.Errorf("wasmsnapshot: EncodeMemory needs a per-trace cache tier")
	}

	nPages := len(memory) / PageSize
	// One region spans the whole memory in the common case; it is split only
	// where a `kind=0` run interrupts the hash runs.
	var (
		cur      *Region
		curRun   *HashRun
		regionAt uint64
	)
	flushRegion := func() {
		if cur != nil {
			regions = append(regions, *cur)
			cur = nil
			curRun = nil
		}
	}

	for i := 0; i < nPages; i++ {
		page := memory[i*PageSize : (i+1)*PageSize]
		off := memBase + uint64(i)*PageSize
		h := HashPage(page)
		_, tier, hit := tiers.Lookup(h)

		if !hit {
			if opts.InlineMissedPages {
				// Close the hash-run region and emit the miss inline.
				flushRegion()
				regions = append(regions, Region{
					Base:       off,
					Size:       PageSize,
					Kind:       KindFull,
					FileOffset: uint64(len(inline)),
				})
				inline = append(inline, page...)
				regionAt = off + PageSize
				continue
			}
			if err := tiers.PerTrace.Insert(h, page); err != nil {
				return nil, nil, err
			}
			tier = CacheIDPerTrace
		}
		if opts.PromoteToSystem && tiers.System != nil {
			if _, present := tiers.System.Lookup(h); !present {
				if err := tiers.System.Insert(h, page); err != nil {
					return nil, nil, fmt.Errorf("wasmsnapshot: promoting a page into the system cache: %w", err)
				}
			}
		}

		if cur == nil {
			regionAt = off
			cur = &Region{Base: regionAt, Kind: KindRunHashRef}
		}
		if curRun == nil || curRun.CacheID != tier {
			cur.Runs = append(cur.Runs, HashRun{CacheID: tier})
			curRun = &cur.Runs[len(cur.Runs)-1]
		}
		curRun.Hashes = append(curRun.Hashes, h)
		cur.Size += PageSize
	}
	flushRegion()
	return regions, inline, nil
}

// MaterialiseMemory reconstructs a linear-memory image from region descriptors
// (CAS spec §3 "Replay algorithm"). `inline` is the `wcpentry.mem` stream.
func MaterialiseMemory(regions []Region, inline []byte, tiers *Tiers, size uint64) ([]byte, error) {
	out := make([]byte, size)
	covered := uint64(0)
	for _, r := range regions {
		if r.Base+r.Size > size {
			return nil, fmt.Errorf(
				"wasmsnapshot: region [%d,%d) runs past the recorded memory size %d",
				r.Base, r.Base+r.Size, size)
		}
		switch r.Kind {
		case KindFull:
			end := r.FileOffset + r.Size
			if end > uint64(len(inline)) {
				return nil, fmt.Errorf(
					"wasmsnapshot: kind=0 region at %d wants wcpentry.mem[%d:%d] but the "+
						"stream is %d bytes", r.Base, r.FileOffset, end, len(inline))
			}
			copy(out[r.Base:r.Base+r.Size], inline[r.FileOffset:end])
			covered += r.Size
		case KindHashRef:
			if err := writePages(out, r.Base, r.Hashes, CacheIDPerTrace, tiers); err != nil {
				return nil, err
			}
			covered += uint64(len(r.Hashes)) * PageSize
		case KindRunHashRef:
			at := r.Base
			for _, run := range r.Runs {
				if err := writePages(out, at, run.Hashes, run.CacheID, tiers); err != nil {
					return nil, err
				}
				at += uint64(len(run.Hashes)) * PageSize
				covered += uint64(len(run.Hashes)) * PageSize
			}
		default:
			return nil, fmt.Errorf(
				"wasmsnapshot: region at %d has unknown kind %d; the snapshot streams "+
					"were written by a newer producer", r.Base, r.Kind)
		}
	}
	if covered != size {
		return nil, fmt.Errorf(
			"wasmsnapshot: the regions cover %d bytes but the snapshot records a "+
				"memory of %d bytes; a partially described memory would replay against "+
				"undefined content", covered, size)
	}
	return out, nil
}

func writePages(out []byte, at uint64, hashes []xxh3.Uint128, cacheID uint64, tiers *Tiers) error {
	for _, h := range hashes {
		page, err := tiers.Resolve(h, cacheID)
		if err != nil {
			return err
		}
		if len(page) != PageSize {
			b := h.Bytes()
			return fmt.Errorf(
				"wasmsnapshot: cached page %s is %d bytes, expected %d",
				hex.EncodeToString(b[:]), len(page), PageSize)
		}
		if at+PageSize > uint64(len(out)) {
			return fmt.Errorf("wasmsnapshot: a hash run writes past the end of the memory image")
		}
		copy(out[at:at+PageSize], page)
		at += PageSize
	}
	return nil
}

// ---------------------------------------------------------------------------
// `wcppages.ns` — the per-trace page store
// ---------------------------------------------------------------------------

// # `wcppages.ns` IS an `NSB1` namespace B-tree
//
// The stream is a real CTFS namespace page image, written and read against
// `codetracer-specs/Trace-Files/CTFS-Binary-Format.md` §10 ("Namespaces"),
// subsection "Key Lookup: B-Tree Index" — which specifies the 61-byte `NSB1`
// header, the leaf entry descriptors **and** the B-tree interior-node layout.
// `nsb1.go` holds the writer and the reader; this file only decides what goes
// into the tree.
//
// (An earlier revision of this file carried a `WPG1` flat sorted table and a
// comment explaining that a wire-compatible namespace could not be written
// because the spec did not describe interior nodes. It does now, so the
// workaround is gone: any CTFS namespace reader can traverse this stream, and
// two independent ones are held to it — a literal transcription of
// `cow_btree.nim`'s reader in `nsb1_crossread_test.go`, and the production Nim
// `loadCowBTree` itself, shelled out to in `nsb1_nim_crossread_test.go`.)
//
// # What is in it
//
// The key is CAS spec §5.4's low-64 truncation of the page's xxh3-128 hash.
// The value is the §5.4 `NamespaceEntry`:
//
//	full_hash:  u8[16]      the whole xxh3-128, so a truncated-key collision
//	                        is detected rather than silently served
//	page_size:  u32 LE
//	reserved:   u32 LE = 0
//	page_bytes: u8[page_size]
//
// Following the convention `codetracer-trace-format-nim`'s
// `linehits_builder.nim` and `memwrites_builder.nim` already use for a
// self-contained namespace image — a namespace B-tree treats descriptors as
// opaque bytes, so a value larger than a descriptor has to live somewhere the
// descriptor points at — the namespace is Leaf Type B with sub-blocks skipped
// (`flags = 0b11`, D = 16, order = 170) and each leaf descriptor is
//
//	[payload_offset u64 LE][payload_len u64 LE]
//
// an **absolute** byte offset into this same stream, pointing past the end of
// the page image at the entry's payload.
//
// The stream is zero-padded to a multiple of 4096, as the Nim builders do and
// as `loadCowBTree` requires ("image not page-aligned"). It differs from them
// in *where* the padding goes: after the page image and **before** the payload
// region, rather than at the end. That is deliberate. Padding at the end means
// the stream's last bytes belong to no record, so the per-page hash check —
// which is the only integrity check this format has — cannot see them, and a
// flipped byte there is invisible. With the padding in front, the payload ends
// exactly at the end of the stream and the last byte of the stream is the last
// byte of a real page, covered by that page's `full_hash`. The descriptors are
// absolute offsets, so the payload region is free to start wherever it likes.
//
// # Collisions
//
// §5.4 says a lookup that finds a key whose `full_hash` differs is "not
// present". That rule would lose a page whose truncated key collides with
// another's, and the per-trace tier is the trace's self-sufficiency guarantee
// (§5.1) — a page it drops is a page no cold-cache replay can find. So the
// payload of one key is a *chain* of one or more `NamespaceEntry` records,
// each self-delimiting through its own `page_size`, and the reader picks the
// record whose `full_hash` matches. With one page per key — every real case at
// this scale, where §5.4 puts the 64-bit collision probability at ~3% for 10M
// entries and a WASM per-trace tier holds thousands — the payload is byte for
// byte a single §5.4 `NamespaceEntry`, so a reader implementing §5.4 literally
// reads exactly what §5.4 describes and reaches §5.4's verdict.
const (
	// pageRecordHeaderSize is `full_hash` + `page_size` + `reserved`.
	pageRecordHeaderSize = 24
	// pageStoreDescSize is the Type B entry descriptor width.
	pageStoreDescSize = nsDescSizeTypeB
	// pageStoreFlags is `NamespaceHeader.flags`: Leaf Type B, sub-blocks
	// skipped. Sub-block allocation is optional per §10 ("appropriate for
	// `threads.ns` where every entry is large") and a 64 KiB page is never a
	// sub-block candidate.
	pageStoreFlags = nsFlagLeafTypeB | nsFlagSkipSubBlocks
)

// # Carrying `SnapshotFormatVersion` through a format that has no version field
//
// `NSB1`'s 4-byte magic is its only version/identity field: §10 says so
// outright, and the container header carries the *container* format version,
// not a stream's. The `wcp.*` streams have their own revision
// (`SnapshotFormatVersion`), and an unrecognised one must keep producing an
// `*UnsupportedVersionError` so `Load` downgrades to "seeking unavailable"
// rather than "recording unreadable" (snapshot spec §6 — the behaviour
// `TestUnknownVersionIsADiagnosticNotAnError` pins on the sibling `wcp.idx`
// stream and `TestAnUnknownPageStoreVersionIsADiagnostic` pins here).
//
// It is carried in a sidecar record inside **page 0**, at byte 64 — after the
// 61-byte `NamespaceHeader` and before the end of a page that §10 reserves
// wholly for the header and never uses as a node. Nothing in the namespace
// format reads those bytes: the Nim `loadCowBTree` and the Rust
// `CowNamespaceReader` both decode the 61 bytes and ignore the rest of page 0,
// which is why the real-Nim cross-read proof passes over an image carrying
// this record. The alternatives were worse — a version in every payload record
// repeats itself once per page and says nothing about an *empty* store, and a
// trailer after the payload has no fixed address to be found at.
var pageStoreSidecarMagic = [4]byte{'W', 'C', 'P', 'V'}

const (
	pageStoreSidecarOffset = 64
	// Layout, all little-endian:
	//   [64..68)  magic "WCPV"
	//   [68..70)  version    u16  — SnapshotFormatVersion
	//   [70..72)  reserved   u16  = 0
	//   [72..76)  page_size  u32  — the CAS page size this store addresses
	//   [76..80)  key_count  u32  — number of namespace keys, cross-checked
	//                               against the traversal
	pageStoreSidecarSize = 16
)

// truncatedKey is CAS spec §5.4's namespace key: the low 64 bits of the
// xxh3-128 page hash.
func truncatedKey(h xxh3.Uint128) uint64 { return h.Lo }

// encodePageStore serialises the per-trace tier as an `NSB1` namespace.
func encodePageStore(c *PerTraceCache) []byte {
	return encodePageStoreKeyed(c, truncatedKey)
}

// encodePageStoreKeyed is `encodePageStore` with the §5.4 truncation injected,
// so a test can force the truncated-key collisions real xxh3-128 hashes will
// not produce at this scale. Production always passes `truncatedKey`.
func encodePageStoreKeyed(c *PerTraceCache, keyOf func(xxh3.Uint128) uint64) []byte {
	// Group the pages by namespace key. A key holds more than one page only
	// under a truncation collision.
	byKey := map[uint64][]xxh3.Uint128{}
	for h := range c.pages {
		byKey[keyOf(h)] = append(byKey[keyOf(h)], h)
	}
	keys := make([]uint64, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, chain := range byKey {
		// A deterministic chain order keeps the stream reproducible; Go's map
		// iteration is not.
		sort.Slice(chain, func(i, j int) bool {
			if chain[i].Hi != chain[j].Hi {
				return chain[i].Hi < chain[j].Hi
			}
			return chain[i].Lo < chain[j].Lo
		})
	}

	// Two-pass sizing, the shape `linehits_builder.nim`'s `buildCowImage` uses:
	// the descriptors are absolute offsets into the finished stream, so the
	// page image has to be sized before they can be computed. Building the
	// tree once over the same keys with zero descriptors gives that size
	// exactly — a bulk-loaded tree's page count is a function of the key count
	// alone, so the sizing tree and the final tree have identical shapes.
	zero := make([]byte, pageStoreDescSize)
	sizing := make([]nsEntry, len(keys))
	for i, k := range keys {
		sizing[i] = nsEntry{key: k, desc: zero}
	}
	sizingImage, err := nsBulkLoad(sizing, pageStoreDescSize, pageStoreFlags)
	if err != nil {
		// Unreachable: `keys` is sorted and deduplicated by construction and
		// every descriptor is `pageStoreDescSize` bytes. Reaching it means this
		// function is wrong, and writing a namespace no reader can traverse is
		// worse than failing loudly at derivation time.
		panic(fmt.Sprintf("wasmsnapshot: sizing the wcppages.ns namespace: %v", err))
	}
	// The page-alignment padding goes between the page image and the payload,
	// so the payload ends flush with the end of the stream; see the header
	// comment for why that placement is load-bearing.
	payloadBytes := 0
	for _, chain := range byKey {
		for _, h := range chain {
			payloadBytes += pageRecordHeaderSize + len(c.pages[h])
		}
	}
	pad := (nsPageSize - payloadBytes%nsPageSize) % nsPageSize
	payloadBase := uint64(len(sizingImage) + pad)

	payload := make([]byte, 0, payloadBytes)
	final := make([]nsEntry, len(keys))
	for i, k := range keys {
		start := payloadBase + uint64(len(payload))
		for _, h := range byKey[k] {
			payload = appendPageRecord(payload, h, c.pages[h])
		}
		desc := make([]byte, pageStoreDescSize)
		binary.LittleEndian.PutUint64(desc[0:], start)
		binary.LittleEndian.PutUint64(desc[8:], payloadBase+uint64(len(payload))-start)
		final[i] = nsEntry{key: k, desc: desc}
	}
	image, err := nsBulkLoad(final, pageStoreDescSize, pageStoreFlags)
	if err != nil {
		panic(fmt.Sprintf("wasmsnapshot: building the wcppages.ns namespace: %v", err))
	}
	if uint64(len(image)+pad) != payloadBase || len(payload) != payloadBytes {
		panic(fmt.Sprintf(
			"wasmsnapshot: the wcppages.ns sizing pass predicted a %d-byte payload at "+
				"offset %d but the final pass produced %d bytes at %d; the descriptors "+
				"would point at the wrong offsets",
			payloadBytes, payloadBase, len(payload), len(image)+pad))
	}

	writePageStoreSidecar(image, len(keys))
	image = append(image, make([]byte, pad)...)
	return append(image, payload...)
}

// appendPageRecord appends one CAS spec §5.4 `NamespaceEntry`.
func appendPageRecord(dst []byte, h xxh3.Uint128, page []byte) []byte {
	hb := h.Bytes()
	dst = append(dst, hb[:]...)
	var sizes [8]byte
	binary.LittleEndian.PutUint32(sizes[0:], uint32(len(page)))
	binary.LittleEndian.PutUint32(sizes[4:], 0) // reserved
	dst = append(dst, sizes[:]...)
	return append(dst, page...)
}

// writePageStoreSidecar stamps the stream-version record into page 0's unused
// tail. See the comment on `pageStoreSidecarMagic` for why it lives there.
func writePageStoreSidecar(image []byte, keyCount int) {
	s := image[pageStoreSidecarOffset:]
	copy(s, pageStoreSidecarMagic[:])
	binary.LittleEndian.PutUint16(s[4:], SnapshotFormatVersion)
	binary.LittleEndian.PutUint16(s[6:], 0)
	binary.LittleEndian.PutUint32(s[8:], PageSize)
	binary.LittleEndian.PutUint32(s[12:], uint32(keyCount))
}

// decodePageStore is the inverse of encodePageStore. Every page's bytes are
// re-hashed: a page store that has been corrupted must fail here, not deliver
// wrong content into a replay.
func decodePageStore(raw []byte) (*PerTraceCache, error) {
	return decodePageStoreKeyed(raw, truncatedKey)
}

// decodePageStoreKeyed is `decodePageStore` with the §5.4 truncation injected;
// see `encodePageStoreKeyed`.
func decodePageStoreKeyed(raw []byte, keyOf func(xxh3.Uint128) uint64) (*PerTraceCache, error) {
	c := NewPerTraceCache()
	// An absent stream is an empty tier, not a broken one.
	if len(raw) == 0 {
		return c, nil
	}
	if len(raw) < nsPageSize {
		return nil, fmt.Errorf(
			"wasmsnapshot: wcppages.ns is %d bytes, too short for its %d-byte "+
				"NSB1 header page", len(raw), nsPageSize)
	}
	// The magic is checked first so a stream that is not a namespace at all
	// says exactly that; the version gate then runs before any structural
	// interpretation, so a stream this build does not understand degrades to
	// "seeking unavailable" instead of being reported as corrupt.
	if [4]byte(raw[0:4]) != nsMagic {
		return nil, fmt.Errorf(
			"wasmsnapshot: wcppages.ns does not carry the %q magic", string(nsMagic[:]))
	}
	s := raw[pageStoreSidecarOffset : pageStoreSidecarOffset+pageStoreSidecarSize]
	if [4]byte(s[0:4]) != pageStoreSidecarMagic {
		return nil, fmt.Errorf(
			"wasmsnapshot: wcppages.ns is an NSB1 namespace but carries no %q version "+
				"record; this build cannot tell which revision of the CAS entry "+
				"encoding its payloads use", string(pageStoreSidecarMagic[:]))
	}
	if v := binary.LittleEndian.Uint16(s[4:]); v != SnapshotFormatVersion {
		return nil, &UnsupportedVersionError{Stream: NamespacePages, Version: v}
	}
	if ps := binary.LittleEndian.Uint32(s[8:]); ps != PageSize {
		return nil, fmt.Errorf(
			"wasmsnapshot: wcppages.ns uses a %d-byte page; this build addresses %d-byte pages",
			ps, PageSize)
	}
	wantKeys := int(binary.LittleEndian.Uint32(s[12:]))

	if flags := raw[nsOffFlags]; flags&nsFlagLeafTypeB == 0 {
		return nil, fmt.Errorf(
			"wasmsnapshot: wcppages.ns declares namespace flags %#02x, i.e. Leaf Type A "+
				"(8-byte descriptors); the page store is written as Leaf Type B", flags)
	}

	entries, err := nsCollect(raw, pageStoreDescSize)
	if err != nil {
		return nil, err
	}
	if len(entries) != wantKeys {
		return nil, fmt.Errorf(
			"wasmsnapshot: wcppages.ns declares %d namespace key(s) but its B-tree "+
				"holds %d", wantKeys, len(entries))
	}

	for _, e := range entries {
		start := binary.LittleEndian.Uint64(e.desc[0:])
		length := binary.LittleEndian.Uint64(e.desc[8:])
		end := start + length
		if end < start || end > uint64(len(raw)) {
			return nil, fmt.Errorf(
				"wasmsnapshot: wcppages.ns entry %#x points at bytes [%d,%d) but the "+
					"stream is %d bytes", e.key, start, end, len(raw))
		}
		chain := raw[start:end]
		// Walk the chain of §5.4 NamespaceEntry records under this key. One
		// record is the universal case; more than one means a truncated-key
		// collision.
		n := 0
		for len(chain) > 0 {
			if len(chain) < pageRecordHeaderSize {
				return nil, fmt.Errorf(
					"wasmsnapshot: wcppages.ns entry %#x has %d trailing byte(s), too "+
						"few for a %d-byte CAS entry header", e.key, len(chain),
					pageRecordHeaderSize)
			}
			var hb [16]byte
			copy(hb[:], chain[0:16])
			h := xxh3.Uint128FromBytes(hb)
			size := int(binary.LittleEndian.Uint32(chain[16:]))
			if size != PageSize {
				return nil, fmt.Errorf(
					"wasmsnapshot: wcppages.ns entry %#x record %d declares a %d-byte "+
						"page; this build addresses %d-byte pages", e.key, n, size, PageSize)
			}
			if len(chain)-pageRecordHeaderSize < size {
				return nil, fmt.Errorf(
					"wasmsnapshot: wcppages.ns entry %#x record %d wants %d byte(s) of "+
						"page content but only %d remain", e.key, n, size,
					len(chain)-pageRecordHeaderSize)
			}
			page := chain[pageRecordHeaderSize : pageRecordHeaderSize+size]
			if HashPage(page) != h {
				return nil, fmt.Errorf(
					"wasmsnapshot: wcppages.ns entry %d does not hash to its own key; the "+
						"page store is corrupt and would deliver wrong bytes into a replay", n)
			}
			// The full hash and the namespace key must agree, or a lookup by
			// key could never reach this page. This is §5.4's structural guard
			// read from the writer's side.
			if keyOf(h) != e.key {
				return nil, fmt.Errorf(
					"wasmsnapshot: wcppages.ns entry %#x record %d carries a full hash "+
						"whose namespace key is %#x; the page is filed under a key no "+
						"lookup would reach", e.key, n, keyOf(h))
			}
			if err := c.Insert(h, page); err != nil {
				return nil, err
			}
			chain = chain[pageRecordHeaderSize+size:]
			n++
		}
		if n == 0 {
			return nil, fmt.Errorf(
				"wasmsnapshot: wcppages.ns entry %#x has an empty payload; a namespace "+
					"key with no CAS entry under it cannot be resolved", e.key)
		}
	}
	return c, nil
}
