package stylus

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/metacraft-labs/trace_record"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// exportFunc reduces boilerplate for building Stylus host hooks. It fetches the
// next event and forwards it to fn.
func exportFunc(mb wazero.HostModuleBuilder, trace *StylusTrace, name string,
	params, results []api.ValueType,
	fn func(m api.Module, stack []uint64, event evmEvent)) wazero.HostModuleBuilder {
	return mb.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(
			func(ctx context.Context, m api.Module, stack []uint64) {
				event, err := trace.nextEvent(name)
				if err != nil {
					panic(fmt.Sprint(err))
				}
				fn(m, stack, event)
			}),
			params, results,
		).Export(name)
}

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
	return exportFunc(mb, trace, "read_args",
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			ptr := uint32(stack[0])
			writeMemoryBytes(mem, ptr, event.outs)
		})
}

func exportWriteResult(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "write_result",
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			ptr := uint32(stack[0])
			_ = readMemoryBytes(mem, ptr, uint32(stack[1]))
			_ = event
		})
}

func exportReadReturnData(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "read_return_data",
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
		[]api.ValueType{api.ValueTypeI32},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			destPtr := uint32(stack[0])
			writeMemoryBytes(mem, destPtr, event.outs)
			stack[0] = uint64(len(event.outs))
		})
}

func exportCreate2(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "create2",
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			codePtr := uint32(stack[0])
			codeLen := uint32(stack[1])
			endowmentPtr := uint32(stack[2])
			saltPtr := uint32(stack[3])
			_ = readMemoryBytes(mem, codePtr, codeLen)
			_ = readMemoryBytes(mem, endowmentPtr, 32)
			_ = readMemoryBytes(mem, saltPtr, 32)
			contractPtr := uint32(stack[4])
			revertPtr := uint32(stack[5])
			writeMemoryBytes(mem, contractPtr, event.outs[:20])
			writeMemoryBytes(mem, revertPtr, event.outs[20:])
		})
}

func exportCreate1(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "create1",
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			codePtr := uint32(stack[0])
			codeLen := uint32(stack[1])
			endowmentPtr := uint32(stack[2])
			_ = readMemoryBytes(mem, codePtr, codeLen)
			_ = readMemoryBytes(mem, endowmentPtr, 32)
			contractPtr := uint32(stack[3])
			revertPtr := uint32(stack[4])
			writeMemoryBytes(mem, contractPtr, event.outs[:20])
			writeMemoryBytes(mem, revertPtr, event.outs[20:])
		})
}

func exportAccountBalance(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "account_balance",
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			addrPtr := uint32(stack[0])
			_ = readMemoryBytes(mem, addrPtr, 20)
			destPtr := uint32(stack[1])
			writeMemoryBytes(mem, destPtr, event.outs)
		})
}

func exportAccountCode(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "account_code",
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32},
		[]api.ValueType{api.ValueTypeI32},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			addrPtr := uint32(stack[0])
			_ = readMemoryBytes(mem, addrPtr, 20)
			destPtr := uint32(stack[3])
			writeMemoryBytes(mem, destPtr, event.outs)
			stack[0] = uint64(len(event.outs))
		})
}

func exportAccountCodeSize(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "account_code_size",
		[]api.ValueType{api.ValueTypeI32},
		[]api.ValueType{api.ValueTypeI32},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			addrPtr := uint32(stack[0])
			_ = readMemoryBytes(mem, addrPtr, 20)
			val := binary.BigEndian.Uint32(event.outs)
			stack[0] = uint64(val)
		})
}

func exportAccountCodehash(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "account_codehash",
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			addrPtr := uint32(stack[0])
			_ = readMemoryBytes(mem, addrPtr, 20)
			destPtr := uint32(stack[1])
			writeMemoryBytes(mem, destPtr, event.outs)
		})
}

func exportReturnDataSize(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "return_data_size",
		[]api.ValueType{}, []api.ValueType{api.ValueTypeI32},
		func(m api.Module, stack []uint64, event evmEvent) {
			val := binary.BigEndian.Uint32(event.outs)
			stack[0] = uint64(val)
		})
}

func exportContractAddress(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "contract_address",
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			ptr := uint32(stack[0])
			writeMemoryBytes(mem, ptr, event.outs)
		})
}

func exportMsgReentrant(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "msg_reentrant",
		[]api.ValueType{}, []api.ValueType{api.ValueTypeI32},
		func(m api.Module, stack []uint64, event evmEvent) {
			val := binary.BigEndian.Uint32(event.outs)
			stack[0] = uint64(val)
		})
}

func exportMsgSender(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "msg_sender",
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			ptr := uint32(stack[0])
			writeMemoryBytes(mem, ptr, event.outs)
		})
}

func exportMsgValue(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "msg_value",
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			ptr := uint32(stack[0])
			writeMemoryBytes(mem, ptr, event.outs)
		})
}

func exportTxInkPrice(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "tx_ink_price",
		[]api.ValueType{}, []api.ValueType{api.ValueTypeI32},
		func(m api.Module, stack []uint64, event evmEvent) {
			val := binary.BigEndian.Uint32(event.outs)
			stack[0] = uint64(val)
		})
}

func exportTxGasPrice(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "tx_gas_price",
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			ptr := uint32(stack[0])
			writeMemoryBytes(mem, ptr, event.outs)
		})
}

func exportTxOrigin(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "tx_origin",
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			ptr := uint32(stack[0])
			writeMemoryBytes(mem, ptr, event.outs)
		})
}

func exportNativeKeccak256(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "native_keccak256",
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			inputPtr := uint32(stack[0])
			inputLen := uint32(stack[1])
			_ = readMemoryBytes(mem, inputPtr, inputLen)
			destPtr := uint32(stack[2])
			writeMemoryBytes(mem, destPtr, event.outs)
		})
}

func exportStorageCacheBytes32(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	eventName := "storage_cache_bytes32"
	return exportFunc(mb, trace, eventName,
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			keyPtr := uint32(stack[0])
			valuePtr := uint32(stack[1])
			key := readMemoryBytes(mem, keyPtr, 32)
			value := fmt.Sprintf("0x%xd", readMemoryBytes(mem, valuePtr, 32))

			_ = event

			metadata := fmt.Sprintf("%s: key 0x%xd", eventName, key)
			record.RegisterRecordEvent(trace_record.EventKindWriteOther, metadata, value)
		})
}

func exportStorageLoadBytes32(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	eventName := "storage_load_bytes32"
	return exportFunc(mb, trace, eventName,
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			keyPtr := uint32(stack[0])
			destPtr := uint32(stack[1])
			key := readMemoryBytes(mem, keyPtr, 32)
			writeMemoryBytes(mem, destPtr, event.outs)

			metadata := fmt.Sprintf("%s: key 0x%xd", eventName, key)
			content := fmt.Sprintf("0x%xd", event.outs)
			record.RegisterRecordEvent(trace_record.EventKindReadOther, metadata, content)
		})
}

func exportStorageFlushCache(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "storage_flush_cache",
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			_ = event
			// This is NOOP
		})
}

func exportEmitLog(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "emit_log",
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			dataPtr := uint32(stack[0])
			len := uint32(stack[1])
			_ = readMemoryBytes(mem, dataPtr, len)
			_ = event
		})
}

func exportCallContract(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "call_contract",
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32},
		[]api.ValueType{api.ValueTypeI32},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			contractPtr := uint32(stack[0])
			dataPtr := uint32(stack[1])
			dataLen := uint32(stack[2])
			valuePtr := uint32(stack[3])
			_ = readMemoryBytes(mem, contractPtr, 20)
			_ = readMemoryBytes(mem, dataPtr, dataLen)
			_ = readMemoryBytes(mem, valuePtr, 32)
			retPtr := uint32(stack[5])
			writeMemoryBytes(mem, retPtr, event.outs[:4])
			stack[0] = uint64(event.outs[4])
		})
}

func exportDelegateCallContract(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "delegate_call_contract",
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32},
		[]api.ValueType{api.ValueTypeI32},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			contractPtr := uint32(stack[0])
			dataPtr := uint32(stack[1])
			dataLen := uint32(stack[2])
			_ = readMemoryBytes(mem, contractPtr, 20)
			_ = readMemoryBytes(mem, dataPtr, dataLen)
			retPtr := uint32(stack[4])
			writeMemoryBytes(mem, retPtr, event.outs[:4])
			stack[0] = uint64(event.outs[4])
		})
}

func exportStaticCallContract(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "static_call_contract",
		[]api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI64, api.ValueTypeI32},
		[]api.ValueType{api.ValueTypeI32},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			contractPtr := uint32(stack[0])
			dataPtr := uint32(stack[1])
			dataLen := uint32(stack[2])
			_ = readMemoryBytes(mem, contractPtr, 20)
			_ = readMemoryBytes(mem, dataPtr, dataLen)
			retPtr := uint32(stack[4])
			writeMemoryBytes(mem, retPtr, event.outs[:4])
			stack[0] = uint64(event.outs[4])
		})
}

func exportBlockBasefee(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "block_basefee",
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			ptr := uint32(stack[0])
			writeMemoryBytes(mem, ptr, event.outs)
		})
}

func exportChainid(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "chainid",
		[]api.ValueType{}, []api.ValueType{api.ValueTypeI64},
		func(m api.Module, stack []uint64, event evmEvent) {
			val := binary.BigEndian.Uint64(event.outs)
			stack[0] = val
		})
}

func exportBlockCoinbase(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "block_coinbase",
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			mem := m.Memory()
			ptr := uint32(stack[0])
			writeMemoryBytes(mem, ptr, event.outs)
		})
}

func exportBlockGasLimit(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "block_gas_limit",
		[]api.ValueType{}, []api.ValueType{api.ValueTypeI64},
		func(m api.Module, stack []uint64, event evmEvent) {
			val := binary.BigEndian.Uint64(event.outs)
			stack[0] = val
		})
}

func exportBlockNumber(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "block_number",
		[]api.ValueType{}, []api.ValueType{api.ValueTypeI64},
		func(m api.Module, stack []uint64, event evmEvent) {
			val := binary.BigEndian.Uint64(event.outs)
			stack[0] = val
		})
}

func exportBlockTimestamp(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "block_timestamp",
		[]api.ValueType{}, []api.ValueType{api.ValueTypeI64},
		func(m api.Module, stack []uint64, event evmEvent) {
			val := binary.BigEndian.Uint64(event.outs)
			stack[0] = val
		})
}

func exportPayForMemoryGrow(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "pay_for_memory_grow",
		[]api.ValueType{api.ValueTypeI32}, []api.ValueType{},
		func(m api.Module, stack []uint64, event evmEvent) {
			_ = event
			// This is NOOP
		})
}

func exportEvmGasLeft(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "evm_gas_left",
		[]api.ValueType{}, []api.ValueType{api.ValueTypeI64},
		func(m api.Module, stack []uint64, event evmEvent) {
			val := binary.BigEndian.Uint64(event.outs)
			stack[0] = val
		})
}

func exportEvmInkLeft(mb wazero.HostModuleBuilder, trace *StylusTrace, record *trace_record.TraceRecord) wazero.HostModuleBuilder {
	return exportFunc(mb, trace, "evm_ink_left",
		[]api.ValueType{}, []api.ValueType{api.ValueTypeI64},
		func(m api.Module, stack []uint64, event evmEvent) {
			val := binary.BigEndian.Uint64(event.outs)
			stack[0] = val
		})
}

func readMemoryBytes(mem api.Memory, ptr uint32, cnt uint32) []byte {
	res, ok := mem.Read(ptr, cnt)
	if !ok {
		panic("Invalid memory access")
	}

	return res
}

func writeMemoryBytes(mem api.Memory, ptr uint32, bytes []byte) {
	if !mem.Write(ptr, bytes) {
		panic("Invalid memory access")
	}
}
