package stylus

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func exportSylusFunctions(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	result := mb
	result = exportReadArgs(result, trace)
	result = exportWriteResult(result, trace)
	result = exportReadReturnData(result, trace)
	result = exportCreate2(result, trace)
	result = exportCreate1(result, trace)
	result = exportAccountBalance(result, trace)
	result = exportAccountCode(result, trace)
	result = exportAccountCodeSize(result, trace)
	result = exportAccountCodehash(result, trace)
	result = exportReturnDataSize(result, trace)
	result = exportContractAddress(result, trace)
	result = exportMsgReentrant(result, trace)
	result = exportMsgSender(result, trace)
	result = exportMsgValue(result, trace)
	result = exportTxInkPrice(result, trace)
	result = exportTxGasPrice(result, trace)
	result = exportTxOrigin(result, trace)
	result = exportNativeKeccak256(result, trace)
	result = exportStorageCacheBytes32(result, trace)
	result = exportStorageLoadBytes32(result, trace)
	result = exportStorageFlushCache(result, trace)
	result = exportEmitLog(result, trace)
	result = exportCallContract(result, trace)
	result = exportDelegateCallContract(result, trace)
	result = exportStaticCallContract(result, trace)
	result = exportBlockBasefee(result, trace)
	result = exportChainid(result, trace)
	result = exportBlockCoinbase(result, trace)
	result = exportBlockGasLimit(result, trace)
	result = exportBlockNumber(result, trace)
	result = exportBlockTimestamp(result, trace)
	result = exportPayForMemoryGrow(result, trace)
	result = exportEvmGasLeft(result, trace)
	result = exportEvmInkLeft(result, trace)

	return result
}

// TODO: do stuff when illegal memory acces
// TODO: what happens when gas or ink runs out

func exportReadArgs(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "read_args"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				mem := m.Memory()

				ptr := uint32(stack[0])

				mem.WriteString(ptr, string(event.outs))
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportWriteResult(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "write_result"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				// TODO: ensure it is Ok to read memory at addr stack[0] for stack[1] bytes

				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportReadReturnData(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "read_return_data"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, m api.Module, stack []uint64) {
			event, err := trace.nextEvent(fname)
			if err != nil {
				panic("TODO")
			}

			panic("TODO")
			_ = event
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export(fname)
}

func exportCreate2(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "create2"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportCreate1(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "create1"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportAccountBalance(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "account_balance"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportAccountCode(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "account_code"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportAccountCodeSize(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "account_code_size"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportAccountCodehash(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "account_codehash"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportReturnDataSize(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "return_data_size"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportContractAddress(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "contract_address"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportMsgReentrant(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "msg_reentrant"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				val, err := byteArrToU32(event.outs)
				if err != nil {
					panic("TODO")
				}

				stack[0] = uint64(val)
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportMsgSender(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "msg_sender"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportMsgValue(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "msg_value"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}
				mem := m.Memory()

				ptr := uint32(stack[0])

				mem.WriteString(ptr, string(event.outs))
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportTxInkPrice(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "tx_ink_price"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportTxGasPrice(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "tx_gas_price"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportTxOrigin(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "tx_origin"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportNativeKeccak256(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "native_keccak256"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportStorageCacheBytes32(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "storage_cache_bytes32"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				_ = event
				// TODO: ensure it is OK to read memoty at addr stack[0] and stack[1]
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportStorageLoadBytes32(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "storage_load_bytes32"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				// TODO: ensure it is OK to read memoty at addr stack[0]

				mem := m.Memory()

				ptr := uint32(stack[1])

				mem.WriteString(ptr, string(event.outs))
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportStorageFlushCache(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "storage_flush_cache"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				_ = event
				// This is NOOP
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportEmitLog(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "emit_log"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportCallContract(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "call_contract"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportDelegateCallContract(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "delegate_call_contract"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportStaticCallContract(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "static_call_contract"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportBlockBasefee(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "block_basefee"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportChainid(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "chainid"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI64},
		).Export(fname)
}

func exportBlockCoinbase(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "block_coinbase"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportBlockGasLimit(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "block_gas_limit"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI64},
		).Export(fname)
}

func exportBlockNumber(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "block_number"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI64},
		).Export(fname)
}

func exportBlockTimestamp(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "block_timestamp"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI64},
		).Export(fname)
}

func exportPayForMemoryGrow(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "pay_for_memory_grow"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportEvmGasLeft(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "evm_gas_left"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI64},
		).Export(fname)
}

func exportEvmInkLeft(mb wazero.HostModuleBuilder, trace *stylusTrace) wazero.HostModuleBuilder {
	fname := "evm_ink_left"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic("TODO")
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI64},
		).Export(fname)
}

func byteArrToU32(arr []byte) (uint32, error) {
	if len(arr) != 4 {
		return 0, fmt.Errorf("not bytes of u32")
	}

	result := uint32(0)
	for _, byte := range arr {
		result *= 0xff
		result += uint32(byte)
	}

	return result, nil
}
