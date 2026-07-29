package boundarylog

import (
	"context"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/testing/require"
)

func TestAppendULEB128(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{624485, []byte{0xe5, 0x8e, 0x26}}, // the canonical LEB128 example
	} {
		require.Equal(t, tc.want, appendULEB128(nil, tc.in), "uleb128(%d)", tc.in)
	}
}

func TestAppendSLEB128(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{-1, []byte{0x7f}},
		{63, []byte{0x3f}},
		{64, []byte{0xc0, 0x00}},
		{-64, []byte{0x40}},
		{-65, []byte{0xbf, 0x7f}},
		{-123456, []byte{0xc0, 0xbb, 0x78}}, // the canonical LEB128 example
	} {
		require.Equal(t, tc.want, appendSLEB128(nil, tc.in), "sleb128(%d)", tc.in)
	}
}

func TestRequiredPages(t *testing.T) {
	require.Equal(t, uint32(0), requiredPages(0))
	require.Equal(t, uint32(1), requiredPages(1))
	require.Equal(t, uint32(1), requiredPages(pageSize))
	require.Equal(t, uint32(2), requiredPages(pageSize+1))
}

// TestProviderModuleIsValidWasm is the real check on the hand-rolled
// encoder: the bytes it produces must be accepted by wazero's own binary
// decoder and validator, and must expose the entities the guest will import.
func TestProviderModuleIsValidWasm(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer func() { require.NoError(t, rt.Close(ctx)) }()

	max := uint32(4)
	spec := &providerSpec{
		module: "env",
		memories: []ImportedMemory{
			{Module: "env", Name: "memory", MinPages: 2, MaxPages: &max},
		},
		globals: []ImportedGlobal{
			{Module: "env", Name: "counter", Type: "i32", Mutable: true, Value: "-7"},
			{Module: "env", Name: "big", Type: "i64", Mutable: false, Value: "9223372036854775807"},
			{Module: "env", Name: "ratio", Type: "f64", Mutable: false, Value: "1.5"},
			{Module: "env", Name: "small", Type: "f32", Mutable: false, Value: "-0.25"},
		},
	}
	binary, err := spec.encode()
	require.NoError(t, err)

	mod, err := rt.InstantiateWithConfig(ctx, binary, wazero.NewModuleConfig().WithName("env"))
	require.NoError(t, err, "the synthesised provider must be valid WebAssembly")

	mem := mod.ExportedMemory("memory")
	require.True(t, mem != nil, "the provider must export the memory")
	require.Equal(t, uint32(2*pageSize), mem.Size())

	counter := mod.ExportedGlobal("counter")
	require.True(t, counter != nil)
	require.Equal(t, api.ValueTypeI32, counter.Type())
	require.Equal(t, int32(-7), int32(uint32(counter.Get())))
	_, mutable := counter.(api.MutableGlobal)
	require.True(t, mutable, "a global declared mutable must be settable")

	big := mod.ExportedGlobal("big")
	require.True(t, big != nil)
	require.Equal(t, uint64(0x7fffffffffffffff), big.Get())

	ratio := mod.ExportedGlobal("ratio")
	require.True(t, ratio != nil)
	require.Equal(t, 1.5, api.DecodeF64(ratio.Get()))

	small := mod.ExportedGlobal("small")
	require.True(t, small != nil)
	require.Equal(t, float32(-0.25), api.DecodeF32(small.Get()))

	immutable := mod.ExportedGlobal("big")
	_, isMutable := immutable.(api.MutableGlobal)
	require.False(t, isMutable, "a global declared immutable must not be settable")
}

// TestProviderModuleReExportsStubs covers the case a name like `env` hits
// routinely: it must present both functions and a memory to the guest, but
// wazero's HostModuleBuilder can only export functions.
func TestProviderModuleReExportsStubs(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer func() { require.NoError(t, rt.Close(ctx)) }()

	const stubs = "internal-stubs"
	called := 0
	_, err := rt.NewHostModuleBuilder(stubs).
		NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
			called++
			stack[0] = 99
		}), []api.ValueType{api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		WithName("tick").Export("tick").
		Instantiate(ctx)
	require.NoError(t, err)

	spec := &providerSpec{
		module:     "env",
		stubModule: stubs,
		memories:   []ImportedMemory{{Module: "env", Name: "memory", MinPages: 1}},
		funcs: []*importPlan{{
			index: 0, module: "env", name: "tick",
			paramTypes: []api.ValueType{api.ValueTypeI32},
			resultTyps: []api.ValueType{api.ValueTypeI32},
		}},
	}
	binary, err := spec.encode()
	require.NoError(t, err)

	mod, err := rt.InstantiateWithConfig(ctx, binary, wazero.NewModuleConfig().WithName("env"))
	require.NoError(t, err)

	require.True(t, mod.ExportedMemory("memory") != nil,
		"the provider must export the memory it defines")

	fn := mod.ExportedFunction("tick")
	require.True(t, fn != nil, "the provider must re-export the imported stub")
	res, err := fn.Call(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, []uint64{99}, res, "the re-exported function must reach the Go stub")
	require.Equal(t, 1, called)
}

// TestProviderTypeSectionIsDeduplicated pins that two functions sharing a
// signature share one functype entry, keeping the section canonical.
func TestProviderTypeSectionIsDeduplicated(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer func() { require.NoError(t, rt.Close(ctx)) }()

	const stubs = "s"
	b := rt.NewHostModuleBuilder(stubs)
	for _, n := range []string{"a", "b", "c"} {
		b = b.NewFunctionBuilder().
			WithGoModuleFunction(api.GoModuleFunc(func(context.Context, api.Module, []uint64) {}),
				[]api.ValueType{api.ValueTypeI32}, nil).
			WithName(n).Export(n)
	}
	_, err := b.Instantiate(ctx)
	require.NoError(t, err)

	spec := &providerSpec{module: "env", stubModule: stubs,
		globals: []ImportedGlobal{{Module: "env", Name: "g", Type: "i32", Value: "0"}}}
	for i, n := range []string{"a", "b", "c"} {
		spec.funcs = append(spec.funcs, &importPlan{
			index: uint32(i), module: "env", name: n,
			paramTypes: []api.ValueType{api.ValueTypeI32},
		})
	}
	binary, err := spec.encode()
	require.NoError(t, err)

	// Section 1 (type) holds a single entry despite three functions.
	require.Equal(t, byte(0x01), binary[8], "section 1 must follow the header")
	require.Equal(t, byte(0x01), binary[10], "the type vector must hold exactly one functype")

	mod, err := rt.InstantiateWithConfig(ctx, binary, wazero.NewModuleConfig().WithName("env"))
	require.NoError(t, err)
	for _, n := range []string{"a", "b", "c"} {
		require.True(t, mod.ExportedFunction(n) != nil, "missing re-export %q", n)
	}
}

func TestEmptyProviderIsRejected(t *testing.T) {
	_, err := (&providerSpec{module: "env"}).encode()
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "would be empty"), "got: %v", err)
}

func TestProviderWithFunctionsButNoStubModuleIsRejected(t *testing.T) {
	spec := &providerSpec{module: "env", funcs: []*importPlan{{name: "f"}}}
	_, err := spec.encode()
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "no stub module name"), "got: %v", err)
}
