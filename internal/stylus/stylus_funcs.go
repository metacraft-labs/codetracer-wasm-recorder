package stylus

import (
	"context"
	"fmt"

	"github.com/metacraft-labs/trace_record"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

func exportSylusFunctions(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	result := mb
	result = exportReadArgs(result, trace, record)
	result = exportWriteResult(result, trace, record)
	result = exportReadReturnData(result, trace, record)
	result = exportCreate2(result, trace, record)
	result = exportCreate1(result, trace, record)
	result = exportAccountBalance(result, trace, record)
	result = exportAccountCode(result, trace, record)
	result = exportAccountCodeSize(result, trace, record)
	result = exportAccountCodehash(result, trace, record)
	result = exportReturnDataSize(result, trace, record)
	result = exportContractAddress(result, trace, record)
	result = exportMsgReentrant(result, trace, record)
	result = exportMsgSender(result, trace, record)
	result = exportMsgValue(result, trace, record)
	result = exportTxInkPrice(result, trace, record)
	result = exportTxGasPrice(result, trace, record)
	result = exportTxOrigin(result, trace, record)
	result = exportNativeKeccak256(result, trace, record)
	result = exportStorageCacheBytes32(result, trace, record)
	result = exportStorageLoadBytes32(result, trace, record)
	result = exportStorageFlushCache(result, trace, record)
	result = exportEmitLog(result, trace, record)
	result = exportCallContract(result, trace, record)
	result = exportDelegateCallContract(result, trace, record)
	result = exportStaticCallContract(result, trace, record)
	result = exportBlockBasefee(result, trace, record)
	result = exportChainid(result, trace, record)
	result = exportBlockCoinbase(result, trace, record)
	result = exportBlockGasLimit(result, trace, record)
	result = exportBlockNumber(result, trace, record)
	result = exportBlockTimestamp(result, trace, record)
	result = exportPayForMemoryGrow(result, trace, record)
	result = exportEvmGasLeft(result, trace, record)
	result = exportEvmInkLeft(result, trace, record)

	return result
}

// TODO: what happens when gas or ink runs out
// TODO: add record logs for events

func exportReadArgs(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "read_args"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				mem := m.Memory()
				ptr := uint32(stack[0])
				writeMemoryBytes(mem, ptr, event.outs)
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportWriteResult(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "write_result"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				mem := m.Memory()
				ptr := uint32(stack[0])
				_ = readMemoryBytes(mem, ptr, uint32(stack[1]))

				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportReadReturnData(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "read_return_data"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{api.ValueTypeI32}).
		Export(fname)
}

func exportCreate2(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "create2"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportCreate1(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "create1"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportAccountBalance(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "account_balance"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportAccountCode(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "account_code"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportAccountCodeSize(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "account_code_size"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportAccountCodehash(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "account_codehash"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportReturnDataSize(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "return_data_size"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportContractAddress(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "contract_address"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportMsgReentrant(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "msg_reentrant"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				val, err := byteArrToU32(event.outs)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				stack[0] = uint64(val)
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportMsgSender(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "msg_sender"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportMsgValue(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "msg_value"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				mem := m.Memory()
				ptr := uint32(stack[0])
				writeMemoryBytes(mem, ptr, event.outs)
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportTxInkPrice(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "tx_ink_price"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportTxGasPrice(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "tx_gas_price"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportTxOrigin(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "tx_origin"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportNativeKeccak256(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "native_keccak256"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportStorageCacheBytes32(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	eventName := "storage_cache_bytes32"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(eventName)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				mem := m.Memory()
				keyPtr := uint32(stack[0])
				valuePtr := uint32(stack[1])
				key := readMemoryBytes(mem, keyPtr, 32)
				value := fmt.Sprintf("0x%xd", readMemoryBytes(mem, valuePtr, 32))

				_ = event
		
				metadata := fmt.Sprintf("%s: key 0x%xd", eventName, key)
				record.RegisterRecordEvent(trace_record.EventKindWriteOther, metadata, value)
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(eventName)
}

func exportStorageLoadBytes32(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	eventName := "storage_load_bytes32"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(eventName)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				mem := m.Memory()
				keyPtr := uint32(stack[0])
				destPtr := uint32(stack[1])
				key := readMemoryBytes(mem, keyPtr, 32)
				writeMemoryBytes(mem, destPtr, event.outs)

				metadata := fmt.Sprintf("%s: key 0x%xd", eventName, key)
				content := fmt.Sprintf("0x%xd", event.outs)
				record.RegisterRecordEvent(trace_record.EventKindReadOther, metadata, content)
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(eventName)
}

func exportStorageFlushCache(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "storage_flush_cache"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				_ = event
				// This is NOOP
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportEmitLog(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "emit_log"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportCallContract(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "call_contract"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportDelegateCallContract(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "delegate_call_contract"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportStaticCallContract(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "static_call_contract"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32},
			[]api.ValueType{api.ValueTypeI32},
		).Export(fname)
}

func exportBlockBasefee(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "block_basefee"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportChainid(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "chainid"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI64},
		).Export(fname)
}

func exportBlockCoinbase(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "block_coinbase"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportBlockGasLimit(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "block_gas_limit"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI64},
		).Export(fname)
}

func exportBlockNumber(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "block_number"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI64},
		).Export(fname)
}

func exportBlockTimestamp(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "block_timestamp"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI64},
		).Export(fname)
}

func exportPayForMemoryGrow(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "pay_for_memory_grow"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{api.ValueTypeI32},
			[]api.ValueType{},
		).Export(fname)
}

func exportEvmGasLeft(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "evm_gas_left"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI64},
		).Export(fname)
}

func exportEvmInkLeft(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	fname := "evm_ink_left"
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(fname)
				if err != nil {
					panic(fmt.Sprint(err))
				}

				panic("TODO")
				_ = event
			}),
			[]api.ValueType{},
			[]api.ValueType{api.ValueTypeI64},
		).Export(fname)
}

func readMemoryBytes(mem api.Memory, ptr uint32, cnt uint32) []byte {
	res, ok := mem.Read(ptr, cnt)
	if !ok {
		panic("Invalid memory acces")
	}

	return res
}

func writeMemoryBytes(mem api.Memory, ptr uint32, bytes []byte) {
	if !mem.Write(ptr, bytes) {
		panic("Invalid memory acces")
	}
}
