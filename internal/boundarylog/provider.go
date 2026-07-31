package boundarylog

import (
	"fmt"
	"math"

	"github.com/tetratelabs/wazero/api"
)

// providerSpec collects everything one host module name must supply: the
// imported memories and globals it exports, and — when the same name also
// supplies imported functions — the stubs it re-exports.
type providerSpec struct {
	module   string
	memories []ImportedMemory
	globals  []ImportedGlobal
	// funcs are the imported functions this module name must also export.
	// A name like `env` routinely supplies both a memory and functions, so
	// the provider has to be able to carry both.
	funcs []*importPlan
	// stubModule is the internal host-module name the function stubs are
	// registered under, and which this provider imports them from. Empty
	// when there are no functions.
	stubModule string
}

// encode builds a minimal WebAssembly module that satisfies one host module
// name for the guest.
//
// wazero's HostModuleBuilder can only export host *functions*; there is no
// public API for exporting a memory or a global from Go. The supported way
// to provide either is to instantiate a real module that defines them, so
// this synthesises the smallest such module.
//
// When the same name must ALSO provide functions, the provider imports the
// Go stubs from an internal host module and re-exports them under their
// original names — a module may export a function it imported, so one
// module can present both kinds of entity to the guest.
//
// The encoding follows the WebAssembly core binary format:
// https://webassembly.github.io/spec/core/binary/modules.html
//
//	magic   = \0 a s m
//	version = 1 (little-endian u32)
//	then a sequence of (id: u8, size: u32 LEB128, contents) sections, in
//	the canonical order — type (1), import (2), memory (5), global (6),
//	export (7).
func (p *providerSpec) encode() ([]byte, error) {
	if len(p.memories) == 0 && len(p.globals) == 0 && len(p.funcs) == 0 {
		return nil, fmt.Errorf("provider module %q would be empty", p.module)
	}

	out := []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

	// --- Type section (id 1) + Import section (id 2) ---------------------
	//
	// One functype per distinct signature, deduplicated so the section
	// stays canonical, plus one import per function this name re-exports.
	if len(p.funcs) > 0 {
		if p.stubModule == "" {
			return nil, fmt.Errorf(
				"provider module %q carries functions but no stub module name", p.module)
		}
		var types [][]byte
		typeIndex := make([]uint32, len(p.funcs))
		for i, f := range p.funcs {
			ft := encodeFuncType(f.paramTypes, f.resultTyps)
			found := -1
			for j, existing := range types {
				if string(existing) == string(ft) {
					found = j
					break
				}
			}
			if found < 0 {
				found = len(types)
				types = append(types, ft)
			}
			typeIndex[i] = uint32(found)
		}

		var typeBody []byte
		typeBody = appendULEB128(typeBody, uint64(len(types)))
		for _, ft := range types {
			typeBody = append(typeBody, ft...)
		}
		out = appendSection(out, 1, typeBody)

		var importBody []byte
		importBody = appendULEB128(importBody, uint64(len(p.funcs)))
		for i, f := range p.funcs {
			importBody = appendName(importBody, p.stubModule)
			importBody = appendName(importBody, f.name)
			importBody = append(importBody, 0x00) // importdesc: func
			importBody = appendULEB128(importBody, uint64(typeIndex[i]))
		}
		out = appendSection(out, 2, importBody)
	}

	// --- Memory section (id 5): vec(limits) ------------------------------
	if len(p.memories) > 0 {
		var body []byte
		body = appendULEB128(body, uint64(len(p.memories)))
		for _, m := range p.memories {
			// limits := 0x00 min | 0x01 min max
			if m.MaxPages == nil {
				body = append(body, 0x00)
				body = appendULEB128(body, uint64(m.MinPages))
			} else {
				body = append(body, 0x01)
				body = appendULEB128(body, uint64(m.MinPages))
				body = appendULEB128(body, uint64(*m.MaxPages))
			}
		}
		out = appendSection(out, 5, body)
	}

	// --- Global section (id 6): vec(globaltype, init expr) ---------------
	if len(p.globals) > 0 {
		var body []byte
		body = appendULEB128(body, uint64(len(p.globals)))
		for _, g := range p.globals {
			t, v, err := g.decode()
			if err != nil {
				return nil, err
			}
			body = append(body, byte(t.ValueType()))
			if g.Mutable {
				body = append(body, 0x01)
			} else {
				body = append(body, 0x00)
			}
			expr, err := constExpr(t, v)
			if err != nil {
				return nil, fmt.Errorf("imported global %s.%s: %w", g.Module, g.Name, err)
			}
			body = append(body, expr...)
		}
		out = appendSection(out, 6, body)
	}

	// --- Export section (id 7): vec(name, kind, index) -------------------
	{
		var body []byte
		body = appendULEB128(body, uint64(len(p.memories)+len(p.globals)+len(p.funcs)))
		// Imported functions occupy the low end of the function index
		// space, so the i-th import is function index i.
		for i, f := range p.funcs {
			body = appendName(body, f.name)
			body = append(body, 0x00) // funcidx
			body = appendULEB128(body, uint64(i))
		}
		for i, m := range p.memories {
			body = appendName(body, m.Name)
			body = append(body, 0x02) // memidx
			body = appendULEB128(body, uint64(i))
		}
		for i, g := range p.globals {
			body = appendName(body, g.Name)
			body = append(body, 0x03) // globalidx
			body = appendULEB128(body, uint64(i))
		}
		out = appendSection(out, 7, body)
	}

	return out, nil
}

// encodeFuncType encodes a `functype`: 0x60, then the parameter and result
// value-type vectors.
func encodeFuncType(params, results []api.ValueType) []byte {
	out := []byte{0x60}
	out = appendULEB128(out, uint64(len(params)))
	for _, v := range params {
		out = append(out, byte(v))
	}
	out = appendULEB128(out, uint64(len(results)))
	for _, v := range results {
		out = append(out, byte(v))
	}
	return out
}

// constExpr encodes a constant initialiser expression for a global:
// one `<t>.const` instruction followed by `end` (0x0B).
func constExpr(t ScalarType, v Value) ([]byte, error) {
	switch t {
	case TypeI32:
		return append(appendSLEB128([]byte{0x41}, int64(int32(uint32(v.Bits)))), 0x0b), nil
	case TypeI64:
		return append(appendSLEB128([]byte{0x42}, int64(v.Bits)), 0x0b), nil
	case TypeF32:
		b := []byte{0x43}
		bits := uint32(v.Bits)
		b = append(b, byte(bits), byte(bits>>8), byte(bits>>16), byte(bits>>24))
		return append(b, 0x0b), nil
	case TypeF64:
		b := []byte{0x44}
		bits := v.Bits
		for i := 0; i < 8; i++ {
			b = append(b, byte(bits>>(8*i)))
		}
		return append(b, 0x0b), nil
	default:
		return nil, fmt.Errorf("cannot encode a constant initialiser for type %s", t)
	}
}

// appendSection appends a section header plus its body.
func appendSection(dst []byte, id byte, body []byte) []byte {
	dst = append(dst, id)
	dst = appendULEB128(dst, uint64(len(body)))
	return append(dst, body...)
}

// appendName appends a WASM `name`: a length-prefixed UTF-8 byte vector.
func appendName(dst []byte, s string) []byte {
	dst = appendULEB128(dst, uint64(len(s)))
	return append(dst, s...)
}

// appendULEB128 appends an unsigned LEB128 integer.
func appendULEB128(dst []byte, v uint64) []byte {
	for {
		b := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b |= 0x80
		}
		dst = append(dst, b)
		if v == 0 {
			return dst
		}
	}
}

// appendSLEB128 appends a signed LEB128 integer.
func appendSLEB128(dst []byte, v int64) []byte {
	for {
		b := byte(v & 0x7f)
		// Arithmetic shift keeps the sign bits coming.
		v >>= 7
		signBitSet := b&0x40 != 0
		if (v == 0 && !signBitSet) || (v == -1 && signBitSet) {
			return append(dst, b)
		}
		dst = append(dst, b|0x80)
	}
}

// pageSize is the WebAssembly linear-memory page size (64 KiB).
const pageSize = 65536

// requiredPages returns the number of pages needed to hold `n` bytes.
func requiredPages(n uint64) uint32 {
	pages := (n + pageSize - 1) / pageSize
	if pages > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(pages)
}
