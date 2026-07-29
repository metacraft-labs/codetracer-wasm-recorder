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

// pageStoreMagic identifies the per-trace page store's on-disk image.
//
// # Why this is not an `NSB1` B-tree
//
// `CTFS-Binary-Format.md` §8 specifies the namespace's 61-byte `NSB1` header
// and its leaf entry descriptors, but **not** the byte layout of B-tree
// interior nodes, so a wire-compatible namespace cannot be written from the
// specification alone — it would have to be reverse-engineered from
// `codetracer-trace-format-nim`'s `cow_btree.nim`, which is outside this
// repository.
//
// Rather than emit something shaped like a namespace that no CTFS reader can
// actually traverse, the store carries its own magic. An `NSB1` reader handed
// this stream fails immediately on the magic check — a legible refusal — where
// a fabricated B-tree would fail somewhere deep in a node walk, or worse,
// return a wrong page. The stream keeps the `wcppages.ns` name because that is
// the name the snapshot spec §6 assigns the per-trace tier and because the
// data is otherwise exactly what a namespace would hold: 64-bit keys (CAS spec
// §5.4's low-64 truncation) mapping to `(full_hash, page_size, page_bytes)`
// entries.
var pageStoreMagic = [4]byte{'W', 'P', 'G', '1'}

const (
	pageStoreHeaderSize = 16
	pageStoreEntrySize  = 40
)

// encodePageStore serialises the per-trace tier.
//
// Entries are sorted by the truncated 64-bit key, so a reader can binary-search
// them — the O(log n) keyed lookup a namespace B-tree provides, without the
// B-tree. The full 128-bit hash is stored alongside each entry exactly as CAS
// spec §5.4 requires, so a truncated-key collision is *detected* rather than
// silently served.
func encodePageStore(c *PerTraceCache) []byte {
	type entry struct {
		key  uint64
		hash xxh3.Uint128
	}
	entries := make([]entry, 0, len(c.pages))
	for h := range c.pages {
		entries = append(entries, entry{key: h.Lo, hash: h})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].key != entries[j].key {
			return entries[i].key < entries[j].key
		}
		return entries[i].hash.Hi < entries[j].hash.Hi
	})

	head := make([]byte, pageStoreHeaderSize+len(entries)*pageStoreEntrySize)
	copy(head, pageStoreMagic[:])
	binary.LittleEndian.PutUint16(head[4:], SnapshotFormatVersion)
	binary.LittleEndian.PutUint16(head[6:], 0)
	binary.LittleEndian.PutUint32(head[8:], PageSize)
	binary.LittleEndian.PutUint32(head[12:], uint32(len(entries)))

	var data []byte
	for i, e := range entries {
		off := pageStoreHeaderSize + i*pageStoreEntrySize
		page := c.pages[e.hash]
		binary.LittleEndian.PutUint64(head[off:], e.key)
		hb := e.hash.Bytes()
		copy(head[off+8:], hb[:])
		binary.LittleEndian.PutUint32(head[off+24:], uint32(len(page)))
		binary.LittleEndian.PutUint32(head[off+28:], 0)
		binary.LittleEndian.PutUint64(head[off+32:], uint64(len(head)+len(data)))
		data = append(data, page...)
	}
	return append(head, data...)
}

// decodePageStore is the inverse of encodePageStore. Every entry's bytes are
// re-hashed: a page store that has been corrupted must fail here, not deliver
// wrong content into a replayed memory.
func decodePageStore(raw []byte) (*PerTraceCache, error) {
	c := NewPerTraceCache()
	if len(raw) == 0 {
		return c, nil
	}
	if len(raw) < pageStoreHeaderSize {
		return nil, fmt.Errorf("wasmsnapshot: wcppages.ns is %d bytes, too short for its header", len(raw))
	}
	if [4]byte(raw[0:4]) != pageStoreMagic {
		return nil, fmt.Errorf(
			"wasmsnapshot: wcppages.ns does not carry the %q magic", string(pageStoreMagic[:]))
	}
	if v := binary.LittleEndian.Uint16(raw[4:]); v != SnapshotFormatVersion {
		return nil, &UnsupportedVersionError{Stream: "wcppages.ns", Version: v}
	}
	if ps := binary.LittleEndian.Uint32(raw[8:]); ps != PageSize {
		return nil, fmt.Errorf(
			"wasmsnapshot: wcppages.ns uses a %d-byte page; this build addresses %d-byte pages",
			ps, PageSize)
	}
	n := int(binary.LittleEndian.Uint32(raw[12:]))
	if pageStoreHeaderSize+n*pageStoreEntrySize > len(raw) {
		return nil, fmt.Errorf("wasmsnapshot: wcppages.ns declares %d entries but is truncated", n)
	}
	for i := 0; i < n; i++ {
		off := pageStoreHeaderSize + i*pageStoreEntrySize
		var hb [16]byte
		copy(hb[:], raw[off+8:off+24])
		h := xxh3.Uint128FromBytes(hb)
		size := int(binary.LittleEndian.Uint32(raw[off+24:]))
		start := binary.LittleEndian.Uint64(raw[off+32:])
		end := start + uint64(size)
		if end > uint64(len(raw)) {
			return nil, fmt.Errorf("wasmsnapshot: wcppages.ns entry %d points past the end of the stream", i)
		}
		page := raw[start:end]
		if HashPage(page) != h {
			return nil, fmt.Errorf(
				"wasmsnapshot: wcppages.ns entry %d does not hash to its own key; the "+
					"page store is corrupt and would deliver wrong bytes into a replay", i)
		}
		if err := c.Insert(h, page); err != nil {
			return nil, err
		}
	}
	return c, nil
}
