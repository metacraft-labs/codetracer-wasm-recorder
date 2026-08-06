package wasmsnapshot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/boundarylog"
	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/xxh3"
)

// No mocks. The page tiers, stream encoders and container writer are all
// exercised against real bytes on a real filesystem; the only synthesised
// inputs are memory images, which are just bytes by construction.

// ---------------------------------------------------------------------------
// Quiescent points
// ---------------------------------------------------------------------------

// writeRecording emits a minimal but *real* browser-shaped `.ct` directory
// with `n` top-level export crossings. It is the same three-file JSON layout
// `browser_stream_host.rs` writes; the richer producer replica lives in
// `internal/boundarylog` and is used by the end-to-end tests there.
func writeRecording(t *testing.T, n int) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "prog.ct")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var events []map[string]any
	events = append(events, map[string]any{"Path": "/src/x.rs"})
	events = append(events, map[string]any{
		"Function": map[string]any{"name": "run", "path_id": float64(0), "line": float64(1)},
	})
	for i := 0; i < n; i++ {
		events = append(events,
			map[string]any{"Call": map[string]any{"function_id": float64(0), "args": []any{}}},
			map[string]any{"Return": map[string]any{
				"return_value": map[string]any{"kind": "None", "type_id": float64(0)}}},
		)
	}
	write := func(name string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("trace.json", events)
	write("trace_metadata.json", map[string]any{
		"program": "prog", "args": []string{}, "workdir": "/w",
		"recorder": map[string]any{"name": "codetracer-js-recorder-browser", "version": "0.1.0"},
	})
	write("trace_paths.json", []string{"/src/x.rs"})
	return dir
}

func TestQuiescentPointsAreDerivedFromTheLogAlone(t *testing.T) {
	for _, n := range []int{0, 1, 5} {
		rec, err := boundarylog.LoadRecording(writeRecording(t, n))
		if err != nil {
			t.Fatalf("LoadRecording: %v", err)
		}
		points, err := QuiescentPoints(rec)
		if err != nil {
			t.Fatalf("QuiescentPoints: %v", err)
		}
		// N exported calls give N+1 quiescent points: one before each call
		// and one after the last.
		if len(points) != n+1 {
			t.Fatalf("%d exported call(s) gave %d quiescent point(s), want %d",
				n, len(points), n+1)
		}
		for i, p := range points {
			if p.Ordinal != i || p.ExportsBefore != i {
				t.Errorf("point %d is %+v", i, p)
			}
		}
		if last := points[len(points)-1]; last.CrossingSeq != -1 {
			t.Errorf("the final point has CrossingSeq %d, want -1", last.CrossingSeq)
		}
		for i := 0; i < n; i++ {
			// Each non-final point precedes the crossing of the call it
			// comes before; with no import crossings the seq is the index.
			if points[i].CrossingSeq != i {
				t.Errorf("point %d precedes crossing %d, want %d", i, points[i].CrossingSeq, i)
			}
			if points[i].NextExport != "run" {
				t.Errorf("point %d names %q", i, points[i].NextExport)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Page CAS
// ---------------------------------------------------------------------------

func page(seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	b := make([]byte, PageSize)
	r.Read(b)
	return b
}

// memoryOf concatenates pages, repeating a page where the same seed appears
// twice — which is what makes the run-length encoding worth having.
func memoryOf(seeds ...int64) []byte {
	var out []byte
	for _, s := range seeds {
		if s < 0 {
			out = append(out, make([]byte, PageSize)...)
			continue
		}
		out = append(out, page(s)...)
	}
	return out
}

func TestMemoryRoundTripsThroughThePerTraceTier(t *testing.T) {
	mem := memoryOf(1, 2, -1, 2, 3, -1, -1)
	tiers := NewTiers(false)

	regions, inline, err := EncodeMemory(mem, tiers, 0, EncodeOptions{})
	if err != nil {
		t.Fatalf("EncodeMemory: %v", err)
	}
	if len(inline) != 0 {
		t.Errorf("the default mode inlined %d byte(s); misses should go to the per-trace tier", len(inline))
	}
	// Four distinct pages (seeds 1, 2, 3 and the zero page).
	if got := tiers.PerTrace.Len(); got != 4 {
		t.Errorf("the per-trace tier holds %d page(s), want 4 distinct ones", got)
	}
	// One region, one run: every page resolved in the same tier.
	if len(regions) != 1 || regions[0].Kind != KindRunHashRef || len(regions[0].Runs) != 1 {
		t.Fatalf("expected one kind=2 region with one run, got %+v", regions)
	}
	if n := len(regions[0].Runs[0].Hashes); n != len(mem)/PageSize {
		t.Errorf("the run covers %d page(s), want %d", n, len(mem)/PageSize)
	}

	back, err := MaterialiseMemory(regions, inline, tiers, uint64(len(mem)))
	if err != nil {
		t.Fatalf("MaterialiseMemory: %v", err)
	}
	if !bytes.Equal(back, mem) {
		t.Error("the materialised memory differs from the original")
	}
}

func TestInlineModeProducesKindZeroRegions(t *testing.T) {
	mem := memoryOf(1, 2, 1)
	tiers := NewTiers(false)

	regions, inline, err := EncodeMemory(mem, tiers, 0, EncodeOptions{InlineMissedPages: true})
	if err != nil {
		t.Fatalf("EncodeMemory: %v", err)
	}
	if tiers.PerTrace.Len() != 0 {
		t.Errorf("inline mode populated the per-trace tier with %d page(s); that would "+
			"store every introduced page twice", tiers.PerTrace.Len())
	}
	// Every page is a miss, so every page becomes its own kind=0 region.
	if len(regions) != 3 {
		t.Fatalf("expected three kind=0 regions, got %d", len(regions))
	}
	for i, r := range regions {
		if r.Kind != KindFull {
			t.Errorf("region %d has kind %d", i, r.Kind)
		}
	}
	if len(inline) != len(mem) {
		t.Errorf("the inline stream is %d bytes, want %d", len(inline), len(mem))
	}
	back, err := MaterialiseMemory(regions, inline, tiers, uint64(len(mem)))
	if err != nil {
		t.Fatalf("MaterialiseMemory: %v", err)
	}
	if !bytes.Equal(back, mem) {
		t.Error("the materialised memory differs from the original")
	}
}

// TestRunsSplitWhenTheResolvingTierChanges is CAS spec §3's "close the current
// run when a cache miss flips the resolution tier".
func TestRunsSplitWhenTheResolvingTierChanges(t *testing.T) {
	root := t.TempDir()
	t.Setenv(CASRootEnv, root)
	sys := OpenSystemCache()

	// Seed the system tier with page 2 only.
	p2 := page(2)
	if err := sys.Insert(HashPage(p2), p2); err != nil {
		t.Fatalf("seeding the system cache: %v", err)
	}

	mem := memoryOf(1, 2, 3)
	tiers := NewTiers(true)
	regions, _, err := EncodeMemory(mem, tiers, 0, EncodeOptions{})
	if err != nil {
		t.Fatalf("EncodeMemory: %v", err)
	}
	if len(regions) != 1 {
		t.Fatalf("expected one region, got %d", len(regions))
	}
	runs := regions[0].Runs
	if len(runs) != 3 {
		t.Fatalf("expected three runs (per-trace, system, per-trace), got %d: %+v", len(runs), runs)
	}
	want := []uint64{CacheIDPerTrace, CacheIDSystem, CacheIDPerTrace}
	for i, r := range runs {
		if r.CacheID != want[i] {
			t.Errorf("run %d resolved in tier %d, want %d", i, r.CacheID, want[i])
		}
		if len(r.Hashes) != 1 {
			t.Errorf("run %d covers %d page(s), want 1", i, len(r.Hashes))
		}
	}
}

// TestAMissAtEveryTierIsFatal is the CAS spec §5 rule that makes the whole
// design safe: "never a silent fallback to best effort".
func TestAMissAtEveryTierIsFatal(t *testing.T) {
	mem := memoryOf(1)
	tiers := NewTiers(false)
	regions, inline, err := EncodeMemory(mem, tiers, 0, EncodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Materialise against an *empty* tier stack — the cold-cache case.
	empty := NewTiers(false)
	_, err = MaterialiseMemory(regions, inline, empty, uint64(len(mem)))
	if err == nil {
		t.Fatal("a page missing from every tier was materialised anyway")
	}
	if !strings.Contains(err.Error(), "present in no cache tier") {
		t.Errorf("unhelpful diagnostic: %v", err)
	}
}

func TestRemoteTierIsRefusedRatherThanGuessed(t *testing.T) {
	mem := memoryOf(1)
	tiers := NewTiers(false)
	regions, _, err := EncodeMemory(mem, tiers, 0, EncodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the run's tier to a remote id, as a commercially produced
	// container that pulled pages from a trace server would.
	regions[0].Runs[0].CacheID = CacheIDFirstRemote
	_, err = MaterialiseMemory(regions, nil, NewTiers(false), uint64(len(mem)))
	if err == nil {
		t.Fatal("a remote-tier reference resolved without a remote")
	}
	if !strings.Contains(err.Error(), "remote cache") {
		t.Errorf("the diagnostic does not mention the remote tier: %v", err)
	}
}

func TestSystemCacheRejectsCorruptedEntries(t *testing.T) {
	root := t.TempDir()
	t.Setenv(CASRootEnv, root)
	sys := OpenSystemCache()
	p := page(9)
	h := HashPage(p)
	if err := sys.Insert(h, p); err != nil {
		t.Fatal(err)
	}
	if _, ok := sys.Lookup(h); !ok {
		t.Fatal("the page was not found straight after insertion")
	}
	// Corrupt it on disk. A cache that served this back would inject wrong
	// bytes into a replay.
	path := sys.pathFor(h)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[123] ^= 0xff
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := sys.Lookup(h); ok {
		t.Error("a corrupted system-cache entry was served")
	}
}

func TestPageStoreRoundTripsAndDetectsCorruption(t *testing.T) {
	c := NewPerTraceCache()
	for i := int64(1); i <= 4; i++ {
		p := page(i)
		if err := c.Insert(HashPage(p), p); err != nil {
			t.Fatal(err)
		}
	}
	raw := encodePageStore(c)
	back, err := decodePageStore(raw)
	if err != nil {
		t.Fatalf("decodePageStore: %v", err)
	}
	if back.Len() != c.Len() {
		t.Fatalf("round trip yielded %d page(s), want %d", back.Len(), c.Len())
	}
	for h, want := range c.pages {
		got, ok := back.Lookup(h)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("page %x did not survive the round trip", h.Lo)
		}
	}

	raw[len(raw)-1] ^= 0xff
	if _, err := decodePageStore(raw); err == nil {
		t.Error("a corrupted page store decoded without complaint")
	}
}

// ---------------------------------------------------------------------------
// Stream encodings
// ---------------------------------------------------------------------------

func TestRegionStreamRoundTripsEveryKind(t *testing.T) {
	h := func(n uint64) xxh3.Uint128 { return xxh3.Uint128{Lo: n, Hi: n * 7} }
	regions := []Region{
		{Base: 0, Size: PageSize, Kind: KindFull, FileOffset: 4096},
		{Base: PageSize, Size: 2 * PageSize, Kind: KindHashRef,
			Hashes: []xxh3.Uint128{h(1), h(2)}},
		{Base: 3 * PageSize, Size: 3 * PageSize, Kind: KindRunHashRef, Runs: []HashRun{
			{CacheID: CacheIDPerTrace, Hashes: []xxh3.Uint128{h(3)}},
			{CacheID: CacheIDSystem, Hashes: []xxh3.Uint128{h(4), h(5)}},
		}},
	}
	raw := encodeRegions(regions)
	back, err := decodeRegions(raw)
	if err != nil {
		t.Fatalf("decodeRegions: %v", err)
	}
	if fmt.Sprint(back) != fmt.Sprint(regions) {
		t.Errorf("round trip changed the regions:\n got %v\nwant %v", back, regions)
	}
}

func TestTruncatedStreamsAreReportedNotPanicked(t *testing.T) {
	raw := encodeRegions([]Region{
		{Base: 0, Size: PageSize, Kind: KindRunHashRef, Runs: []HashRun{
			{CacheID: 0, Hashes: []xxh3.Uint128{{Lo: 1}}},
		}},
	})
	for cut := 1; cut < len(raw); cut++ {
		if _, err := decodeRegions(raw[:cut]); err == nil {
			t.Fatalf("a stream truncated to %d of %d bytes decoded cleanly", cut, len(raw))
		}
	}
}

func TestGlobalAndTableStreamsRoundTrip(t *testing.T) {
	gs := []GlobalValue{
		{ValueType: api.ValueTypeI32, Mutable: true, Lo: 0x1234},
		{ValueType: api.ValueTypeF64, Lo: 0xdeadbeefcafe},
		{ValueType: wasm.ValueTypeV128, Mutable: true, Lo: 1, Hi: 2},
	}
	back, err := decodeGlobals(encodeGlobals(gs))
	if err != nil {
		t.Fatalf("decodeGlobals: %v", err)
	}
	if fmt.Sprint(back) != fmt.Sprint(gs) {
		t.Errorf("globals round trip: got %v, want %v", back, gs)
	}

	ts := []TableValue{
		{RefType: 0x70, Elements: []int32{0, NullElement, 3}},
		{RefType: 0x70, Elements: nil},
	}
	tback, err := decodeTables(encodeTables(ts))
	if err != nil {
		t.Fatalf("decodeTables: %v", err)
	}
	if len(tback) != len(ts) {
		t.Fatalf("tables round trip gave %d table(s), want %d", len(tback), len(ts))
	}
	if fmt.Sprint(tback[0]) != fmt.Sprint(ts[0]) {
		t.Errorf("table 0 round trip: got %v, want %v", tback[0], ts[0])
	}
}

func TestIndexStreamRoundTrips(t *testing.T) {
	recs := []IndexRecord{
		{Ordinal: 0, ExportsBefore: 0, CrossingSeq: 0, Flags: flagHasSnapshot | flagBaseSnapshot,
			MemoryBytes: PageSize, LayLength: 12},
		{Ordinal: 1, ExportsBefore: 1, CrossingSeq: -1},
	}
	back, err := decodeIndex(encodeIndex(recs))
	if err != nil {
		t.Fatalf("decodeIndex: %v", err)
	}
	if fmt.Sprint(back) != fmt.Sprint(recs) {
		t.Errorf("index round trip: got %v, want %v", back, recs)
	}
	if !back[0].HasSnapshot() || !back[0].IsBase() {
		t.Error("flags did not survive the round trip")
	}
	if back[1].HasSnapshot() {
		t.Error("a point with no snapshot came back claiming one")
	}
}

// TestUnknownVersionIsADiagnosticNotAnError is the narrowed version gate of
// snapshot spec §6, at the stream level.
func TestUnknownVersionIsADiagnosticNotAnError(t *testing.T) {
	raw := encodeIndex([]IndexRecord{{Ordinal: 0, CrossingSeq: -1}})
	raw[4] = byte(SnapshotFormatVersion + 1)
	_, err := decodeIndex(raw)
	uv, ok := err.(*UnsupportedVersionError)
	if !ok {
		t.Fatalf("an unknown version produced %T: %v", err, err)
	}
	if !strings.Contains(uv.Error(), "linear replay") {
		t.Errorf("the diagnostic does not say what happens instead: %v", uv)
	}
}

// ---------------------------------------------------------------------------
// Container attachment
// ---------------------------------------------------------------------------

func TestBuilderAttachesAdditiveNamespaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.ct")
	c, err := ctfs.Create(path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	original := map[string][]byte{"meta.dat": []byte("hello"), "steps.dat": bytes.Repeat([]byte{7}, 9000)}
	if err := c.AddFiles(original); err != nil {
		t.Fatal(err)
	}

	points := []QuiescentPoint{{Ordinal: 0, CrossingSeq: 0}, {Ordinal: 1, CrossingSeq: -1}}
	b, err := NewBuilder(points, NewTiers(false), EncodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mem := memoryOf(1, 2)
	if err := b.Add(&Snapshot{Point: 0, MemoryBytes: uint64(len(mem)), Memory: mem,
		Globals: []GlobalValue{{ValueType: api.ValueTypeI32, Mutable: true, Lo: 5}}}); err != nil {
		t.Fatal(err)
	}
	mem2 := memoryOf(1, 3)
	if err := b.Add(&Snapshot{Point: 1, MemoryBytes: uint64(len(mem2)), Memory: mem2,
		Globals: []GlobalValue{{ValueType: api.ValueTypeI32, Mutable: true, Lo: 9}}}); err != nil {
		t.Fatal(err)
	}
	if err := b.MarkBase(0); err != nil {
		t.Fatal(err)
	}
	if err := b.AttachTo(path); err != nil {
		t.Fatalf("AttachTo: %v", err)
	}

	// Additive: nothing pre-existing changed.
	re, err := ctfs.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range original {
		got, err := re.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%q changed when the snapshot namespaces were attached", name)
		}
	}

	set, diag, err := Load(path, false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if diag != "" {
		t.Fatalf("unexpected diagnostic: %s", diag)
	}
	if set.SnapshotCount() != 2 {
		t.Errorf("Load found %d snapshot(s), want 2", set.SnapshotCount())
	}
	// The three distinct pages (seeds 1, 2, 3) are each stored once, even
	// though page 1 appears in both snapshots — the point of the CAS.
	rec, ok := set.Nearest(1)
	if !ok || rec.Ordinal != 1 {
		t.Fatalf("Nearest(1) = %+v, %v", rec, ok)
	}
	snap, err := set.Snapshot(rec)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !bytes.Equal(snap.Memory, mem2) {
		t.Error("the second snapshot's memory did not round-trip")
	}
	if len(snap.Globals) != 1 || snap.Globals[0].Lo != 9 {
		t.Errorf("globals did not round-trip: %+v", snap.Globals)
	}
	first, ok := set.Nearest(0)
	if !ok || !first.IsBase() {
		t.Error("the slice base flag did not survive")
	}
}

// TestLoadOnAContainerWithoutSnapshotsIsNotAnError: a `.ct` with no `snap*`
// namespaces is a complete, valid recording (snapshot spec §6).
func TestLoadOnAContainerWithoutSnapshotsIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.ct")
	c, err := ctfs.Create(path, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddFiles(map[string][]byte{"meta.dat": []byte("x")}); err != nil {
		t.Fatal(err)
	}
	set, diag, err := Load(path, false)
	if err != nil {
		t.Fatalf("Load returned an error for a snapshot-free container: %v", err)
	}
	if set != nil {
		t.Error("Load invented a snapshot set")
	}
	if !strings.Contains(diag, "linear replay") {
		t.Errorf("the diagnostic does not say what happens instead: %q", diag)
	}
}

// TestMarkBaseRefusesAPointWithoutASnapshot: a slice is only independently
// materialisable because its base is a complete resume point.
func TestMarkBaseRefusesAPointWithoutASnapshot(t *testing.T) {
	b, err := NewBuilder([]QuiescentPoint{{Ordinal: 0, CrossingSeq: -1}}, NewTiers(false), EncodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.MarkBase(0); err == nil {
		t.Error("a point carrying no snapshot was marked as a slice base")
	}
}

func TestNamespaceNamesFitBase40(t *testing.T) {
	for _, n := range NamespaceNames() {
		if _, err := ctfs.EncodeName(n); err != nil {
			t.Errorf("namespace %q cannot be a CTFS internal filename: %v", n, err)
		}
	}
}
