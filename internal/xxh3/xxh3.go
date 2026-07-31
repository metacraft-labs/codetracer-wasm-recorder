// Package xxh3 implements XXH3-128 with the default secret and a zero seed —
// the exact variant `MCR-Memory-Page-CAS.md` §4 selects for content-addressing
// memory pages ("xxh3-128 … 128-bit output: collision-free at the working-set
// scales we expect … no cryptographic resistance needed").
//
// # Why a hand-written port
//
// The recorder repo vendors no Go hash dependency and has no network access
// during development, so the algorithm is ported here rather than imported.
// The port follows the reference implementation, xxHash v0.8.3
// (`xxhash.h`, BSD-2-Clause, https://github.com/Cyan4973/xxHash), function for
// function; every routine below names its reference counterpart. The
// specification of the algorithm itself is
// https://github.com/Cyan4973/xxHash/blob/dev/doc/xxhash_spec.md.
//
// `xxh3_test.go` pins the port against digests produced by that reference C
// implementation, so a transcription error fails loudly rather
// than silently producing a self-consistent but wrong content address. That
// matters more here than raw speed: a page CAS whose hashes disagree with
// every other CodeTracer component would deduplicate against nothing, and —
// worse — two components disagreeing about a page's identity is precisely the
// "stale or wrong bytes" failure the CAS design forbids.
//
// # Scope
//
// Only the one-shot 128-bit digest with the default secret and seed 0 is
// implemented, because that is the only variant the page CAS uses. The seeded
// and custom-secret variants, the streaming API, and the 64-bit digest are
// deliberately absent rather than stubbed: an unused code path that nothing
// pins against the reference is a liability.
package xxh3

import (
	"encoding/binary"
	"math/bits"
)

// Uint128 is a 128-bit digest. `Lo` is the reference implementation's
// `XXH128_hash_t.low64` and `Hi` its `.high64`.
//
// `MCR-Memory-Page-CAS.md` §5.4 keys the CTFS namespace on "the low 64 bits"
// of the digest and stores the full 128 bits inside the entry to detect the
// truncation collision, so both halves are exposed.
type Uint128 struct {
	Lo uint64
	Hi uint64
}

// Bytes returns the canonical 16-byte little-endian encoding used on the wire:
// the low 64 bits first, then the high 64 bits. This is the layout
// `MCR-Memory-Page-CAS.md` §3 calls `u8[16]` in a `kind=1` / `kind=2` hash
// list and §5.4 calls `full_hash: u8[16]`.
func (h Uint128) Bytes() [16]byte {
	var b [16]byte
	binary.LittleEndian.PutUint64(b[0:8], h.Lo)
	binary.LittleEndian.PutUint64(b[8:16], h.Hi)
	return b
}

// Uint128FromBytes is the inverse of Bytes.
func Uint128FromBytes(b [16]byte) Uint128 {
	return Uint128{
		Lo: binary.LittleEndian.Uint64(b[0:8]),
		Hi: binary.LittleEndian.Uint64(b[8:16]),
	}
}

// Reference: XXH_PRIME32_* / XXH_PRIME64_* and PRIME_MX* in xxhash.h.
const (
	prime32_1 = 0x9E3779B1
	prime32_2 = 0x85EBCA77
	prime32_3 = 0xC2B2AE3D
	prime32_4 = 0x27D4EB2F
	prime32_5 = 0x165667B1

	prime64_1 = 0x9E3779B185EBCA87
	prime64_2 = 0xC2B2AE3D27D4EB4F
	prime64_3 = 0x165667B19E3779F9
	prime64_4 = 0x85EBCA77C2B2AE63
	prime64_5 = 0x27D4EB2F165667C5

	primeMX1 = 0x165667919E3779F9
	primeMX2 = 0x9FB21C651E98DF25
)

// Reference: XXH_STRIPE_LEN, XXH_SECRET_CONSUME_RATE, XXH_ACC_NB,
// XXH_SECRET_MERGEACCS_START, XXH_SECRET_LASTACC_START, XXH3_MIDSIZE_MAX,
// XXH3_MIDSIZE_STARTOFFSET, XXH3_MIDSIZE_LASTOFFSET, XXH3_SECRET_SIZE_MIN.
const (
	stripeLen          = 64
	secretConsumeRate  = 8
	accNB              = stripeLen / 8
	secretMergeAccsPos = 11
	secretLastAccPos   = 7
	midsizeMax         = 240
	midsizeStartOffset = 3
	midsizeLastOffset  = 17
	secretSizeMin      = 136
)

// kSecret is XXH3_kSecret from xxhash.h — the 192-byte default secret.
var kSecret = [192]byte{
	0xb8, 0xfe, 0x6c, 0x39, 0x23, 0xa4, 0x4b, 0xbe, 0x7c, 0x01, 0x81, 0x2c, 0xf7, 0x21, 0xad, 0x1c,
	0xde, 0xd4, 0x6d, 0xe9, 0x83, 0x90, 0x97, 0xdb, 0x72, 0x40, 0xa4, 0xa4, 0xb7, 0xb3, 0x67, 0x1f,
	0xcb, 0x79, 0xe6, 0x4e, 0xcc, 0xc0, 0xe5, 0x78, 0x82, 0x5a, 0xd0, 0x7d, 0xcc, 0xff, 0x72, 0x21,
	0xb8, 0x08, 0x46, 0x74, 0xf7, 0x43, 0x24, 0x8e, 0xe0, 0x35, 0x90, 0xe6, 0x81, 0x3a, 0x26, 0x4c,
	0x3c, 0x28, 0x52, 0xbb, 0x91, 0xc3, 0x00, 0xcb, 0x88, 0xd0, 0x65, 0x8b, 0x1b, 0x53, 0x2e, 0xa3,
	0x71, 0x64, 0x48, 0x97, 0xa2, 0x0d, 0xf9, 0x4e, 0x38, 0x19, 0xef, 0x46, 0xa9, 0xde, 0xac, 0xd8,
	0xa8, 0xfa, 0x76, 0x3f, 0xe3, 0x9c, 0x34, 0x3f, 0xf9, 0xdc, 0xbb, 0xc7, 0xc7, 0x0b, 0x4f, 0x1d,
	0x8a, 0x51, 0xe0, 0x4b, 0xcd, 0xb4, 0x59, 0x31, 0xc8, 0x9f, 0x7e, 0xc9, 0xd9, 0x78, 0x73, 0x64,
	0xea, 0xc5, 0xac, 0x83, 0x34, 0xd3, 0xeb, 0xc3, 0xc5, 0x81, 0xa0, 0xff, 0xfa, 0x13, 0x63, 0xeb,
	0x17, 0x0d, 0xdd, 0x51, 0xb7, 0xf0, 0xda, 0x49, 0xd3, 0x16, 0x55, 0x26, 0x29, 0xd4, 0x68, 0x9e,
	0x2b, 0x16, 0xbe, 0x58, 0x7d, 0x47, 0xa1, 0xfc, 0x8f, 0xf8, 0xb8, 0xd1, 0x7a, 0xd0, 0x31, 0xce,
	0x45, 0xcb, 0x3a, 0x8f, 0x95, 0x16, 0x04, 0x28, 0xaf, 0xd7, 0xfb, 0xca, 0xbb, 0x4b, 0x40, 0x7e,
}

func readLE32(b []byte) uint32 { return binary.LittleEndian.Uint32(b) }
func readLE64(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }

// Reference: XXH64_avalanche.
func avalanche64(h uint64) uint64 {
	h ^= h >> 33
	h *= prime64_2
	h ^= h >> 29
	h *= prime64_3
	h ^= h >> 32
	return h
}

// Reference: XXH_xorshift64.
func xorshift64(v uint64, shift uint) uint64 { return v ^ (v >> shift) }

// Reference: XXH3_avalanche.
func avalanche3(h uint64) uint64 {
	h = xorshift64(h, 37)
	h *= primeMX1
	h = xorshift64(h, 32)
	return h
}

// Reference: XXH3_mul128_fold64.
func mul128Fold64(lhs, rhs uint64) uint64 {
	hi, lo := bits.Mul64(lhs, rhs)
	return lo ^ hi
}

// Reference: XXH3_mix16B.
func mix16B(input, secret []byte, seed uint64) uint64 {
	return mul128Fold64(
		readLE64(input)^(readLE64(secret)+seed),
		readLE64(input[8:])^(readLE64(secret[8:])-seed),
	)
}

// Reference: XXH128_mix32B.
func mix32B(acc Uint128, input1, input2, secret []byte, seed uint64) Uint128 {
	acc.Lo += mix16B(input1, secret, seed)
	acc.Lo ^= readLE64(input2) + readLE64(input2[8:])
	acc.Hi += mix16B(input2, secret[16:], seed)
	acc.Hi ^= readLE64(input1) + readLE64(input1[8:])
	return acc
}

// Hash128 returns the XXH3-128 digest of `data` with the default secret and
// seed 0 — i.e. the reference implementation's `XXH3_128bits(data, len)`.
func Hash128(data []byte) Uint128 {
	secret := kSecret[:]
	switch n := len(data); {
	case n <= 16:
		return len0to16(data, secret)
	case n <= 128:
		return len17to128(data, secret)
	case n <= midsizeMax:
		return len129to240(data, secret)
	default:
		return hashLong(data, secret)
	}
}

// Reference: XXH3_len_0to16_128b (seed == 0 throughout, so the seed terms
// vanish; they are kept written out so the port stays diffable against the
// reference).
func len0to16(data, secret []byte) Uint128 {
	const seed = 0
	switch n := len(data); {
	case n > 8:
		return len9to16(data, secret)
	case n >= 4:
		return len4to8(data, secret)
	case n > 0:
		return len1to3(data, secret)
	default:
		bitflipl := readLE64(secret[64:]) ^ readLE64(secret[72:])
		bitfliph := readLE64(secret[80:]) ^ readLE64(secret[88:])
		return Uint128{
			Lo: avalanche64(seed ^ bitflipl),
			Hi: avalanche64(seed ^ bitfliph),
		}
	}
}

// Reference: XXH3_len_1to3_128b.
func len1to3(data, secret []byte) Uint128 {
	const seed = 0
	n := len(data)
	c1 := uint32(data[0])
	c2 := uint32(data[n>>1])
	c3 := uint32(data[n-1])
	combinedl := (c1 << 16) | (c2 << 24) | (c3 << 0) | (uint32(n) << 8)
	combinedh := bits.RotateLeft32(bits.ReverseBytes32(combinedl), 13)
	bitflipl := uint64(readLE32(secret)^readLE32(secret[4:])) + seed
	bitfliph := uint64(readLE32(secret[8:])^readLE32(secret[12:])) - seed
	return Uint128{
		Lo: avalanche64(uint64(combinedl) ^ bitflipl),
		Hi: avalanche64(uint64(combinedh) ^ bitfliph),
	}
}

// Reference: XXH3_len_4to8_128b.
func len4to8(data, secret []byte) Uint128 {
	const seed = 0
	n := len(data)
	inputLo := readLE32(data)
	inputHi := readLE32(data[n-4:])
	input64 := uint64(inputLo) + (uint64(inputHi) << 32)
	bitflip := (readLE64(secret[16:]) ^ readLE64(secret[24:])) + seed
	keyed := input64 ^ bitflip

	hi, lo := bits.Mul64(keyed, prime64_1+(uint64(n)<<2))
	hi += lo << 1
	lo ^= hi >> 3
	lo = xorshift64(lo, 35)
	lo *= primeMX2
	lo = xorshift64(lo, 28)
	return Uint128{Lo: lo, Hi: avalanche3(hi)}
}

// Reference: XXH3_len_9to16_128b (64-bit branch of the `input_hi` fold).
func len9to16(data, secret []byte) Uint128 {
	const seed = 0
	n := len(data)
	bitflipl := (readLE64(secret[32:]) ^ readLE64(secret[40:])) - seed
	bitfliph := (readLE64(secret[48:]) ^ readLE64(secret[56:])) + seed
	inputLo := readLE64(data)
	inputHi := readLE64(data[n-8:])

	mHi, mLo := bits.Mul64(inputLo^inputHi^bitflipl, prime64_1)
	mLo += uint64(n-1) << 54
	inputHi ^= bitfliph
	mHi += inputHi + uint64(uint32(inputHi))*(prime32_2-1)
	mLo ^= bits.ReverseBytes64(mHi)

	hHi, hLo := bits.Mul64(mLo, prime64_2)
	hHi += mHi * prime64_2
	return Uint128{Lo: avalanche3(hLo), Hi: avalanche3(hHi)}
}

// Reference: XXH3_len_17to128_128b.
func len17to128(data, secret []byte) Uint128 {
	const seed = 0
	n := len(data)
	acc := Uint128{Lo: uint64(n) * prime64_1}
	if n > 32 {
		if n > 64 {
			if n > 96 {
				acc = mix32B(acc, data[48:], data[n-64:], secret[96:], seed)
			}
			acc = mix32B(acc, data[32:], data[n-48:], secret[64:], seed)
		}
		acc = mix32B(acc, data[16:], data[n-32:], secret[32:], seed)
	}
	acc = mix32B(acc, data, data[n-16:], secret, seed)
	return foldMidsize(acc, uint64(n), seed)
}

// Reference: XXH3_len_129to240_128b.
func len129to240(data, secret []byte) Uint128 {
	const seed = 0
	n := len(data)
	acc := Uint128{Lo: uint64(n) * prime64_1}
	for i := 32; i < 160; i += 32 {
		acc = mix32B(acc, data[i-32:], data[i-16:], secret[i-32:], seed)
	}
	acc.Lo = avalanche3(acc.Lo)
	acc.Hi = avalanche3(acc.Hi)
	// NB: `i <= n` duplicates the last 32 bytes when n%32 == 0. The
	// reference calls this "an unfortunate necessity to keep the hash result
	// stable"; reproducing it is mandatory for compatibility.
	for i := 160; i <= n; i += 32 {
		acc = mix32B(acc, data[i-32:], data[i-16:], secret[midsizeStartOffset+i-160:], seed)
	}
	acc = mix32B(acc, data[n-16:], data[n-32:],
		secret[secretSizeMin-midsizeLastOffset-16:], 0-uint64(seed))
	return foldMidsize(acc, uint64(n), seed)
}

// foldMidsize is the shared tail of XXH3_len_17to128_128b and
// XXH3_len_129to240_128b.
func foldMidsize(acc Uint128, n, seed uint64) Uint128 {
	lo := acc.Lo + acc.Hi
	hi := acc.Lo*prime64_1 + acc.Hi*prime64_4 + (n-seed)*prime64_2
	return Uint128{Lo: avalanche3(lo), Hi: 0 - avalanche3(hi)}
}

// initAcc is XXH3_INIT_ACC.
var initAcc = [accNB]uint64{
	prime32_3, prime64_1, prime64_2, prime64_3,
	prime64_4, prime32_2, prime64_5, prime32_1,
}

// accumulate512 is XXH3_accumulate_512_scalar: one 64-byte stripe.
func accumulate512(acc *[accNB]uint64, input, secret []byte) {
	for i := 0; i < accNB; i++ {
		dataVal := readLE64(input[i*8:])
		dataKey := dataVal ^ readLE64(secret[i*8:])
		acc[i^1] += dataVal // swap adjacent lanes
		acc[i] += uint64(uint32(dataKey)) * uint64(uint32(dataKey>>32))
	}
}

// accumulate is XXH3_accumulate (the XXH3_ACCUMULATE_TEMPLATE expansion).
func accumulate(acc *[accNB]uint64, input, secret []byte, nbStripes int) {
	for n := 0; n < nbStripes; n++ {
		accumulate512(acc, input[n*stripeLen:], secret[n*secretConsumeRate:])
	}
}

// scrambleAcc is XXH3_scrambleAcc_scalar.
func scrambleAcc(acc *[accNB]uint64, secret []byte) {
	for i := 0; i < accNB; i++ {
		key64 := readLE64(secret[i*8:])
		v := acc[i]
		v = xorshift64(v, 47)
		v ^= key64
		v *= prime32_1
		acc[i] = v
	}
}

// Reference: XXH3_hashLong_internal_loop.
func hashLongLoop(acc *[accNB]uint64, input, secret []byte) {
	n := len(input)
	secretSize := len(secret)
	nbStripesPerBlock := (secretSize - stripeLen) / secretConsumeRate
	blockLen := stripeLen * nbStripesPerBlock
	nbBlocks := (n - 1) / blockLen

	for i := 0; i < nbBlocks; i++ {
		accumulate(acc, input[i*blockLen:], secret, nbStripesPerBlock)
		scrambleAcc(acc, secret[secretSize-stripeLen:])
	}

	// last partial block
	nbStripes := ((n - 1) - blockLen*nbBlocks) / stripeLen
	accumulate(acc, input[nbBlocks*blockLen:], secret, nbStripes)

	// last stripe
	accumulate512(acc, input[n-stripeLen:], secret[secretSize-stripeLen-secretLastAccPos:])
}

// Reference: XXH3_mix2Accs.
func mix2Accs(acc []uint64, secret []byte) uint64 {
	return mul128Fold64(acc[0]^readLE64(secret), acc[1]^readLE64(secret[8:]))
}

// Reference: XXH3_mergeAccs.
func mergeAccs(acc *[accNB]uint64, secret []byte, start uint64) uint64 {
	result := start
	for i := 0; i < 4; i++ {
		result += mix2Accs(acc[2*i:], secret[16*i:])
	}
	return avalanche3(result)
}

// Reference: XXH3_hashLong_128b_internal + XXH3_finalizeLong_128b.
func hashLong(input, secret []byte) Uint128 {
	acc := initAcc
	hashLongLoop(&acc, input, secret)
	secretSize := len(secret)
	n := uint64(len(input))
	return Uint128{
		Lo: mergeAccs(&acc, secret[secretMergeAccsPos:], n*prime64_1),
		Hi: mergeAccs(&acc, secret[secretSize-stripeLen-secretMergeAccsPos:], ^(n * prime64_2)),
	}
}
