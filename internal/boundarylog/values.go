package boundarylog

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/tetratelabs/wazero/api"
)

// ScalarType is one of the four WebAssembly numeric types a boundary value
// can carry. Reference types (`externref` / `funcref`) and `v128` are
// deliberately absent: the instrumenter rejects a boundary whose signature
// mentions one rather than recording it with a gap (spec §8), so a
// recording can never contain one.
//
// The string forms are the ones `ct-instrument` emits in its manifest —
// lowercase, matching `ScalarType`'s `#[serde(rename_all = "lowercase")]`
// in `codetracer-wasm-instrumenter/crates/codetracer-wasm-instrumenter/src/manifest.rs`.
type ScalarType uint8

const (
	// TypeI32 is a 32-bit integer — recorded via `__ct_emit_i32`.
	TypeI32 ScalarType = iota
	// TypeI64 is a 64-bit integer — recorded via `__ct_emit_i64`.
	TypeI64
	// TypeF32 is a 32-bit float — recorded via `__ct_emit_f32`.
	TypeF32
	// TypeF64 is a 64-bit float — recorded via `__ct_emit_f64`.
	TypeF64
)

// String returns the manifest spelling of the type.
func (t ScalarType) String() string {
	switch t {
	case TypeI32:
		return "i32"
	case TypeI64:
		return "i64"
	case TypeF32:
		return "f32"
	case TypeF64:
		return "f64"
	default:
		return fmt.Sprintf("ScalarType(%d)", uint8(t))
	}
}

// ParseScalarType maps a manifest type spelling onto a ScalarType. Anything
// outside the four numeric types — including the `v128` and reference-type
// spellings `ct-instrument` uses in its `unsupportedType` diagnostics — is
// rejected loudly rather than silently coerced (spec §8).
func ParseScalarType(s string) (ScalarType, error) {
	switch s {
	case "i32":
		return TypeI32, nil
	case "i64":
		return TypeI64, nil
	case "f32":
		return TypeF32, nil
	case "f64":
		return TypeF64, nil
	default:
		return 0, fmt.Errorf("unsupported boundary value type %q: "+
			"only i32/i64/f32/f64 can cross a recorded boundary (spec §8)", s)
	}
}

// ScalarTypeOf maps a wazero api.ValueType onto a ScalarType, so signatures
// can be taken from the module's own type section when no manifest is
// supplied.
func ScalarTypeOf(v api.ValueType) (ScalarType, error) {
	switch v {
	case api.ValueTypeI32:
		return TypeI32, nil
	case api.ValueTypeI64:
		return TypeI64, nil
	case api.ValueTypeF32:
		return TypeF32, nil
	case api.ValueTypeF64:
		return TypeF64, nil
	default:
		return 0, fmt.Errorf("unsupported boundary value type %s: "+
			"only i32/i64/f32/f64 can cross a recorded boundary (spec §8)",
			api.ValueTypeName(v))
	}
}

// ValueType returns the wazero api.ValueType this ScalarType denotes.
func (t ScalarType) ValueType() api.ValueType {
	switch t {
	case TypeI32:
		return api.ValueTypeI32
	case TypeI64:
		return api.ValueTypeI64
	case TypeF32:
		return api.ValueTypeF32
	default:
		return api.ValueTypeF64
	}
}

// Signature is a boundary edge's parameter and result types, in order.
type Signature struct {
	Params  []ScalarType
	Results []ScalarType
}

// String renders the signature the way the spec's tables do, for use in
// divergence diagnostics.
func (s Signature) String() string {
	render := func(ts []ScalarType) string {
		parts := make([]string, len(ts))
		for i, t := range ts {
			parts[i] = t.String()
		}
		return strings.Join(parts, ", ")
	}
	return "(" + render(s.Params) + ") -> (" + render(s.Results) + ")"
}

// Equal reports whether two signatures are identical.
func (s Signature) Equal(o Signature) bool {
	if len(s.Params) != len(o.Params) || len(s.Results) != len(o.Results) {
		return false
	}
	for i := range s.Params {
		if s.Params[i] != o.Params[i] {
			return false
		}
	}
	for i := range s.Results {
		if s.Results[i] != o.Results[i] {
			return false
		}
	}
	return true
}

// Value is a single typed value that crossed a boundary. `Bits` holds the
// wazero-native `uint64` encoding for `Type` — the same representation
// `api.Function.Call` takes and returns, so no conversion is needed at the
// call site.
type Value struct {
	Type ScalarType
	Bits uint64
}

// String renders the value for divergence diagnostics: the decoded number
// plus, for floats, the raw bit pattern (spec §7 makes a NaN payload
// mismatch a divergence, so the diagnostic has to show payloads).
func (v Value) String() string {
	switch v.Type {
	case TypeI32:
		return fmt.Sprintf("i32:%d", int32(uint32(v.Bits)))
	case TypeI64:
		return fmt.Sprintf("i64:%d", int64(v.Bits))
	case TypeF32:
		f := api.DecodeF32(v.Bits)
		return fmt.Sprintf("f32:%v (0x%08x)", f, uint32(v.Bits))
	case TypeF64:
		f := api.DecodeF64(v.Bits)
		return fmt.Sprintf("f64:%v (0x%016x)", f, v.Bits)
	default:
		return fmt.Sprintf("<invalid type %d>:0x%x", uint8(v.Type), v.Bits)
	}
}

// Equal reports whether two values are the same type and the same bits.
//
// Comparison is over the raw bits deliberately: `-0.0` must not compare
// equal to `+0.0`, and two NaNs with different payloads must not compare
// equal, because spec §7 lists a NaN payload mismatch as a divergence.
func (v Value) Equal(o Value) bool {
	return v.Type == o.Type && v.Bits == o.Bits
}

// FromWazero wraps a raw wazero stack word as a typed Value.
func FromWazero(t ScalarType, raw uint64) Value {
	return Value{Type: t, Bits: raw}
}

// rawValue is a value as it appears in a `.ct` trace's `ValueRecord`
// encoding: a `kind` discriminant plus a decimal-string payload. See
// `codetracer/src/backend-manager/src/browser_stream_receiver.rs`
// (`ValueRecordOnDisk`) for the producing definition.
type rawValue struct {
	// Kind is the `ValueRecordOnDisk` variant name: "Int", "Float",
	// "Raw", "Bool", "String" or "None".
	Kind string
	// Text is the payload string: `i` for Int, `f` for Float, `r` for Raw.
	Text string
}

func (r rawValue) String() string {
	return fmt.Sprintf("%s(%s)", r.Kind, r.Text)
}

// decode turns a recorded `.ct` value into a typed Value, given the type
// the boundary's signature says the slot carries.
//
// The mapping is dictated by two producers:
//
//   - `browser_session.js` tags an `i32` as `typeKind:"Int"`, an `f32`/`f64`
//     as `"Float"`, and an `i64` as `"BigInt"` carrying an *exact decimal
//     string* (a JS `Number` would round anything above 2^53).
//   - `browser_stream_receiver.rs::translate_value` maps `"Int"`→`Int`,
//     `"Float"`→`Float`, and **everything it does not recognise — including
//     `"BigInt"` — to `Raw`**. So a recorded `i64` reaches disk as
//     `{"kind":"Raw","r":"<exact decimal>"}`.
//
// Both `Int` and `Raw` are therefore accepted for the integer types.
//
// KNOWN LOSS (spec §7): the browser path carries floats as JSON numbers, so
// a NaN *payload* does not survive the recording. `codetracer-wasm-
// instrumenter/recorder-runtime/host_runtime.js` documents the same limit.
// Replay cannot recover what was never recorded; it compares whatever the
// recording does carry.
func (r rawValue) decode(t ScalarType) (Value, error) {
	switch t {
	case TypeI32, TypeI64:
		switch r.Kind {
		case "Int", "Raw":
		default:
			return Value{}, fmt.Errorf(
				"recorded value %s cannot be read as %s: expected a ValueRecord "+
					"of kind Int or Raw (an i64 reaches disk as Raw because the "+
					"browser tags it BigInt)", r, t)
		}
		text := strings.TrimSpace(r.Text)
		// Accept both signed and unsigned spellings: the browser masks an
		// i32 with `| 0` (so it may be negative) while an i64 arrives as
		// the decimal expansion of a JS BigInt, which for a u64 above
		// 2^63-1 is larger than math.MaxInt64.
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			if t == TypeI32 {
				return Value{Type: t, Bits: uint64(uint32(int32(n)))}, nil
			}
			return Value{Type: t, Bits: uint64(n)}, nil
		}
		if u, err := strconv.ParseUint(text, 10, 64); err == nil {
			if t == TypeI32 {
				return Value{Type: t, Bits: uint64(uint32(u))}, nil
			}
			return Value{Type: t, Bits: u}, nil
		}
		return Value{}, fmt.Errorf("recorded value %s is not a valid %s integer", r, t)

	case TypeF32, TypeF64:
		if r.Kind != "Float" {
			return Value{}, fmt.Errorf(
				"recorded value %s cannot be read as %s: expected a ValueRecord "+
					"of kind Float", r, t)
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(r.Text), 64)
		if err != nil {
			return Value{}, fmt.Errorf("recorded value %s is not a valid %s float: %w", r, t, err)
		}
		if t == TypeF32 {
			// f32 -> f64 -> f32 is exact for every finite f32 and both
			// infinities, so the browser's widening to a JS Number is
			// lossless here. NaN payloads are the documented exception.
			return Value{Type: t, Bits: uint64(math.Float32bits(float32(f)))}, nil
		}
		return Value{Type: t, Bits: math.Float64bits(f)}, nil

	default:
		return Value{}, fmt.Errorf("unsupported boundary value type %s", t)
	}
}

// decodeTuple decodes a positional run of recorded values against a
// signature's type list. A length mismatch is a hard error: it means the
// recording and the module disagree about the boundary's arity, which
// spec §6 classes as a missing capture rather than something to paper over.
func decodeTuple(kindLabel string, raws []rawValue, types []ScalarType) ([]Value, error) {
	if len(raws) != len(types) {
		return nil, fmt.Errorf(
			"recorded %s tuple has %d value(s) but the boundary signature declares %d",
			kindLabel, len(raws), len(types))
	}
	out := make([]Value, len(raws))
	for i := range raws {
		v, err := raws[i].decode(types[i])
		if err != nil {
			return nil, fmt.Errorf("%s slot %d: %w", kindLabel, i, err)
		}
		out[i] = v
	}
	return out, nil
}

// formatValues renders a value tuple for a divergence diagnostic.
func formatValues(vs []Value) string {
	parts := make([]string, len(vs))
	for i, v := range vs {
		parts[i] = v.String()
	}
	return "(" + strings.Join(parts, ", ") + ")"
}
