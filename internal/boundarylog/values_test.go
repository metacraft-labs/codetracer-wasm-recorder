package boundarylog

import (
	"math"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/testing/require"
)

func TestParseScalarTypeAcceptsTheManifestSpellings(t *testing.T) {
	for spelling, want := range map[string]ScalarType{
		"i32": TypeI32, "i64": TypeI64, "f32": TypeF32, "f64": TypeF64,
	} {
		got, err := ParseScalarType(spelling)
		require.NoError(t, err, "parsing %q", spelling)
		require.Equal(t, want, got)
		require.Equal(t, spelling, got.String())
	}
}

// TestParseScalarTypeRejectsUnrepresentableTypes pins spec §8: a boundary
// whose signature mentions a reference type or v128 is rejected, not
// recorded with a gap. The instrumenter refuses at record time; this is the
// replay side of the same rule.
func TestParseScalarTypeRejectsUnrepresentableTypes(t *testing.T) {
	for _, spelling := range []string{"v128", "externref", "funcref", "a reference type", "", "I32"} {
		_, err := ParseScalarType(spelling)
		require.Error(t, err, "type %q must be rejected", spelling)
	}
}

func TestScalarTypeOfMapsWazeroValueTypes(t *testing.T) {
	for vt, want := range map[api.ValueType]ScalarType{
		api.ValueTypeI32: TypeI32,
		api.ValueTypeI64: TypeI64,
		api.ValueTypeF32: TypeF32,
		api.ValueTypeF64: TypeF64,
	} {
		got, err := ScalarTypeOf(vt)
		require.NoError(t, err)
		require.Equal(t, want, got)
		require.Equal(t, vt, got.ValueType())
	}
	_, err := ScalarTypeOf(api.ValueTypeExternref)
	require.Error(t, err, "externref must be rejected (spec §8)")
}

// TestDecodeIntegersFromTheBrowserEncoding pins the two encodings the
// browser path actually produces for integers.
func TestDecodeIntegersFromTheBrowserEncoding(t *testing.T) {
	tests := []struct {
		name string
		raw  rawValue
		typ  ScalarType
		bits uint64
	}{
		// `browser_session.js` masks an i32 with `| 0`, so a u32 above
		// 2^31 arrives as a negative decimal and must be re-widened.
		{"i32 positive", rawValue{"Int", "42"}, TypeI32, 42},
		{"i32 negative", rawValue{"Int", "-1"}, TypeI32, 0xffffffff},
		{"i32 min", rawValue{"Int", "-2147483648"}, TypeI32, 0x80000000},
		{"i32 as unsigned decimal", rawValue{"Int", "4294967295"}, TypeI32, 0xffffffff},
		// An i64 reaches disk as Raw because the browser tags it BigInt
		// and `translate_value` does not recognise that kind.
		{"i64 via Raw", rawValue{"Raw", "9223372036854775807"}, TypeI64, 0x7fffffffffffffff},
		{"i64 negative via Raw", rawValue{"Raw", "-1"}, TypeI64, 0xffffffffffffffff},
		{"u64 above MaxInt64", rawValue{"Raw", "18446744073709551615"}, TypeI64, 0xffffffffffffffff},
		{"i64 via Int", rawValue{"Int", "620"}, TypeI64, 620},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := tc.raw.decode(tc.typ)
			require.NoError(t, err)
			require.Equal(t, tc.typ, v.Type)
			require.Equal(t, tc.bits, v.Bits)
		})
	}
}

func TestDecodeFloats(t *testing.T) {
	v, err := rawValue{"Float", "1.5"}.decode(TypeF64)
	require.NoError(t, err)
	require.Equal(t, 1.5, api.DecodeF64(v.Bits))

	v, err = rawValue{"Float", "1.5"}.decode(TypeF32)
	require.NoError(t, err)
	require.Equal(t, float32(1.5), api.DecodeF32(v.Bits))

	// Negative zero must survive: it is a distinct bit pattern and
	// `Value.Equal` compares bits, so a -0.0/+0.0 confusion would be a
	// silently wrong replay.
	v, err = rawValue{"Float", "-0"}.decode(TypeF64)
	require.NoError(t, err)
	require.Equal(t, math.Float64bits(math.Copysign(0, -1)), v.Bits)
	require.False(t, v.Equal(Value{Type: TypeF64, Bits: math.Float64bits(0)}),
		"-0.0 must not compare equal to +0.0")
}

// TestDecodeRejectsTheWrongValueKind pins that a recording whose value
// kinds do not match the signature is refused rather than coerced.
func TestDecodeRejectsTheWrongValueKind(t *testing.T) {
	_, err := rawValue{"Float", "1.5"}.decode(TypeI32)
	require.Error(t, err, "a Float record must not be read as an i32")

	_, err = rawValue{"Int", "3"}.decode(TypeF64)
	require.Error(t, err, "an Int record must not be read as an f64")

	_, err = rawValue{"String", "hello"}.decode(TypeI32)
	require.Error(t, err, "a String record must not be read as an i32")

	_, err = rawValue{"Int", "not-a-number"}.decode(TypeI32)
	require.Error(t, err)
}

func TestDecodeTupleChecksArity(t *testing.T) {
	_, err := decodeTuple("argument",
		[]rawValue{{"Int", "1"}},
		[]ScalarType{TypeI32, TypeI32})
	require.Error(t, err, "a short tuple must be rejected")

	got, err := decodeTuple("argument",
		[]rawValue{{"Int", "1"}, {"Int", "2"}},
		[]ScalarType{TypeI32, TypeI32})
	require.NoError(t, err)
	require.Equal(t, 2, len(got))
}

// TestValueEqualComparesBitsNotNumbers pins spec §7: a NaN payload
// mismatch is a divergence, so equality is over raw bits.
func TestValueEqualComparesBitsNotNumbers(t *testing.T) {
	quiet := Value{Type: TypeF64, Bits: math.Float64bits(math.NaN())}
	payload := Value{Type: TypeF64, Bits: math.Float64bits(math.NaN()) | 0x3}
	require.False(t, quiet.Equal(payload),
		"two NaNs with different payloads must not compare equal (spec §7)")
	require.True(t, quiet.Equal(quiet))
	require.False(t, Value{Type: TypeI32, Bits: 1}.Equal(Value{Type: TypeI64, Bits: 1}),
		"values of different types must never compare equal")
}

func TestSignatureRendering(t *testing.T) {
	s := Signature{Params: []ScalarType{TypeI32, TypeF64}, Results: []ScalarType{TypeI64}}
	require.Equal(t, "(i32, f64) -> (i64)", s.String())
	require.Equal(t, "() -> ()", Signature{}.String())
	require.True(t, s.Equal(Signature{
		Params: []ScalarType{TypeI32, TypeF64}, Results: []ScalarType{TypeI64}}))
	require.False(t, s.Equal(Signature{Params: []ScalarType{TypeI32}, Results: []ScalarType{TypeI64}}))
}
