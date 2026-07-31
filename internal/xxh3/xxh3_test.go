package xxh3

import (
	"encoding/binary"
	"testing"
)

// fill is the deterministic filler the reference-vector generator used, so the
// Go side hashes exactly the bytes the C side hashed without the test having
// to carry megabytes of literal input. It is an xorshift64* stream, chosen
// only because it is trivially reproducible in both languages.
//
// The generator is reproduced verbatim in `TestVectorsMatchReferenceC`'s doc
// comment so the pin can be regenerated.
func fill(b []byte) {
	s := uint64(0x9E3779B97F4A7C15)
	for i := range b {
		s ^= s >> 12
		s ^= s << 25
		s ^= s >> 27
		b[i] = byte((s * 0x2545F4914F6CDD1D) >> 56)
	}
}

// referenceVectors pins this port against xxHash v0.8.3's own
// `XXH3_128bits()`. They were produced by compiling the reference
// implementation and printing the digest of `fill()`ed buffers at each length:
//
//	#define XXH_STATIC_LINKING_ONLY
//	#define XXH_IMPLEMENTATION
//	#include "xxhash.h"
//	/* ...same fill() as above, same length table... */
//	XXH128_hash_t h = XXH3_128bits(buf, len);
//	printf("{%zu, 0x%016llx, 0x%016llx},\n", len, h.low64, h.high64);
//
// The lengths deliberately straddle every branch of the algorithm: the
// 0/1-3/4-8/9-16 short cases, the 17-128 and 129-240 mid cases, and the
// long path either side of the 240-byte cutoff, the 64-byte stripe, the
// 1024-byte block (16 stripes × 64 B), and the 64 KiB WASM page the memory
// CAS actually hashes.
var referenceVectors = []struct {
	n      int
	lo, hi uint64
}{
	{0, 0x6001c324468d497f, 0x99aa06d3014798d8},
	{1, 0x8a21d78b1538b1c0, 0x79d2c79e874f72cd},
	{2, 0xcfce42e3290a1262, 0xe4dca7b84b8da954},
	{3, 0xe40336b30bf2a78b, 0xac7b793459e5a563},
	{4, 0x903a8d0b327eb26b, 0xf30b53443f9a591f},
	{5, 0x92d7a3e612a69454, 0xfd88929f5a697826},
	{7, 0x33a8d40a06b1d291, 0x4c431f3e2c3ed292},
	{8, 0x1dd06c257aa49405, 0x8168d4efe83859e2},
	{9, 0xfb7b419afbb5f529, 0x9e71572fd20ce2ad},
	{12, 0xd2e101aa61337f65, 0xedb6e8029cad27e2},
	{16, 0x0b1e31a2980b0a50, 0x19760481f90ba4b0},
	{17, 0x47ae08bc5713b76f, 0x4c2e3049513c6fee},
	{31, 0x8c0bb7bb8b063162, 0x3fa67de0c98a3973},
	{32, 0xff20a559fbffd6cd, 0x382e3ed6a810f0e7},
	{63, 0x7d9e260a1c13194a, 0x64006b23d4fc9101},
	{64, 0x02d935b2a5c1af72, 0x2dbca9bb864a1837},
	{96, 0x420ba296d60f3074, 0x8842f74c0461cd8a},
	{127, 0x94acbfe0d6cc8c6f, 0x88e0219b59c999b3},
	{128, 0x947bfbdbd4cbc1cf, 0x5e7f91e172195021},
	{129, 0xdb5607ab0dd9a932, 0xedefce7bcdcc0df2},
	{160, 0x5413e99966c21a07, 0x00c4c9fb02003813},
	{191, 0xea5c6459fc1b14b6, 0x6e1eb9d0bc0bab86},
	{192, 0xa0190a73c6ef70a6, 0x0fa4680a6c61a564},
	{239, 0x633a5384b4a0bfae, 0x05744c9578ce105c},
	{240, 0x3449a58fc523b36a, 0x15eecc2209998c59},
	{241, 0xa964e00aaaf73f82, 0x205f88c0a835a0df},
	{255, 0x42d0c1cfbd6e0b7b, 0x25e12ea8c087306f},
	{256, 0xc4a9b6b1a6060acf, 0x36e396d1670a1735},
	{511, 0x5c445530f9950f6a, 0x81ddddae566ca0e3},
	{512, 0x19e3229c994a6902, 0xa1a656d5cb867661},
	{1023, 0xb3fba4019284b01a, 0xc5ac33ec088772f9},
	{1024, 0xc942e27bbdfd1428, 0xe537d5c886668d24},
	{4096, 0x6f91a494cc74a6bc, 0xb4f0252f33fe7cdc},
	{65535, 0x6cab592f49ecd77b, 0xd6428e057c65d632},
	{65536, 0xe2829e66c4a08f0e, 0x6395c2b62dbaf59a},
	{65537, 0x488a0776c8e7ad93, 0xc60df7b28bc1310f},
	{131072, 0xe657139f3d224dbe, 0x927aab06bc7d7362},
	{262144, 0x274fb5f3710c31fc, 0x77221f7003ef6548},
}

// TestVectorsMatchReferenceC is the whole justification for hand-porting the
// algorithm. Without it the port would be self-consistent and possibly wrong,
// which for a content-addressable store is the worst of both worlds: it would
// deduplicate against nothing and disagree with every other CodeTracer
// component about a page's identity.
func TestVectorsMatchReferenceC(t *testing.T) {
	buf := make([]byte, 300000)
	fill(buf)
	for _, v := range referenceVectors {
		got := Hash128(buf[:v.n])
		if got.Lo != v.lo || got.Hi != v.hi {
			t.Errorf("Hash128(len=%d) = {lo:%#016x hi:%#016x}, reference says {lo:%#016x hi:%#016x}",
				v.n, got.Lo, got.Hi, v.lo, v.hi)
		}
	}
}

// TestEmptyInputMatchesTheDocumentedConstant is a second, independent pin:
// XXH3_128bits("") is a widely published constant, so it catches a
// mis-transcribed default secret even if the generator above were rerun
// against a damaged xxhash.h.
func TestEmptyInputMatchesTheDocumentedConstant(t *testing.T) {
	got := Hash128(nil)
	if got.Lo != 0x6001c324468d497f || got.Hi != 0x99aa06d3014798d8 {
		t.Fatalf("Hash128(\"\") = {lo:%#016x hi:%#016x}", got.Lo, got.Hi)
	}
}

func TestBytesRoundTrip(t *testing.T) {
	h := Uint128{Lo: 0x0123456789abcdef, Hi: 0xfedcba9876543210}
	b := h.Bytes()
	if got := binary.LittleEndian.Uint64(b[0:8]); got != h.Lo {
		t.Errorf("low half encoded as %#x", got)
	}
	if got := binary.LittleEndian.Uint64(b[8:16]); got != h.Hi {
		t.Errorf("high half encoded as %#x", got)
	}
	if back := Uint128FromBytes(b); back != h {
		t.Errorf("round trip produced %+v, want %+v", back, h)
	}
}

// TestDistinctPagesHashDistinctly is a sanity floor rather than a collision
// study: a single flipped byte anywhere in a 64 KiB page must change the
// digest. A CAS whose hash ignored part of its input would silently return
// the wrong page.
func TestDistinctPagesHashDistinctly(t *testing.T) {
	const pageSize = 64 * 1024
	page := make([]byte, pageSize)
	fill(page)
	base := Hash128(page)
	for _, off := range []int{0, 1, 63, 64, 1023, 4095, pageSize / 2, pageSize - 2, pageSize - 1} {
		page[off] ^= 0xff
		if got := Hash128(page); got == base {
			t.Errorf("flipping byte %d did not change the digest", off)
		}
		page[off] ^= 0xff
	}
	if Hash128(page) != base {
		t.Fatal("restoring the page did not restore its digest")
	}
}
