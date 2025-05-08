package interpreter

import (
	"debug/dwarf"
	"encoding/binary"
	"fmt"

	"github.com/metacraft-labs/trace_record"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/wasmdebug"
)

func readVariable(m *wasm.ModuleInstance, v wasmdebug.VariableRecord, functionRecord wasmdebug.FunctionRecord, locals []uint64) (trace_record.ValueRecord, error) {
	memAddr := uint32(locals[functionRecord.FrameBaseIndex] + v.Offset)
	memSize := uint32(v.Type.Size())

	mem := m.Memory()

	rawBytes, ok := mem.Read(memAddr, memSize)
	if !ok {
		return nil, fmt.Errorf("out of range memory access")
	}

	return bytesToValueRecord(rawBytes, v.Type, m)
}

func bytesToValueRecord(rawBytes []byte, typ dwarf.Type, m *wasm.ModuleInstance) (val trace_record.ValueRecord, err error) {

	switch t := typ.(type) {
	case *dwarf.IntType:
		val, err = bytesToInt(rawBytes, t, m)

	case *dwarf.UintType:
		val, err = bytesToUint(rawBytes, t, m)

	case *dwarf.BoolType:
		val, err = bytesToBool(rawBytes, t, m)

	case *dwarf.StructType:
		val, err = bytesToStruct(rawBytes, t, m)

	case *dwarf.PtrType:
		val, err = bytesToPointer(rawBytes, t, m)

	default:
		fmt.Printf("WE HAVE SOMETHING ELSE: %T %#v\n", t, t)
		// TODO
		val = trace_record.NilValue()
	}

	return
}

func bytesToInt(rawBytes []byte, typ *dwarf.IntType, m *wasm.ModuleInstance) (trace_record.ValueRecord, error) {
	size := typ.ByteSize
	var intVal int64

	record := m.Record

	switch size {
	case 1:
		intVal = int64(int8(rawBytes[0]))

	case 2:
		intVal = int64(int16(binary.LittleEndian.Uint16(rawBytes)))

	case 4:
		intVal = int64(int32(binary.LittleEndian.Uint32(rawBytes)))

	case 8:
		intVal = int64(binary.LittleEndian.Uint64(rawBytes))

	default:
		return nil, fmt.Errorf("unsupported int variable byte size %v", size)
	}

	// TODO: what should the string parameter be?
	intTypeRecord := trace_record.NewSimpleTypeRecord(trace_record.INT_TYPE_KIND, "Int")
	typeId := record.RegisterTypeWithNewId(typ.Name, intTypeRecord)

	return trace_record.IntValue(intVal, typeId), nil
}

func bytesToUint(rawBytes []byte, typ *dwarf.UintType, m *wasm.ModuleInstance) (trace_record.ValueRecord, error) {
	size := typ.ByteSize
	var intVal uint64

	record := m.Record

	switch size {
	case 1:
		intVal = uint64(rawBytes[0])

	case 2:
		intVal = uint64(binary.LittleEndian.Uint16(rawBytes))

	case 4:
		intVal = uint64(binary.LittleEndian.Uint32(rawBytes))

	case 8:
		intVal = binary.LittleEndian.Uint64(rawBytes)

	default:
		return nil, fmt.Errorf("unsupported uint variable byte size %v", size)
	}

	// TODO: what should the string parameter be?
	intTypeRecord := trace_record.NewSimpleTypeRecord(trace_record.INT_TYPE_KIND, "Uint")
	typeId := record.RegisterTypeWithNewId(typ.Name, intTypeRecord)

	// TODO: discuss int64 uint64 stuff?
	return trace_record.IntValue(int64(intVal), typeId), nil
}

func bytesToBool(rawBytes []byte, typ *dwarf.BoolType, m *wasm.ModuleInstance) (trace_record.ValueRecord, error) {
	size := typ.ByteSize
	var boolVal bool

	record := m.Record

	switch size {
	case 1:
		boolVal = rawBytes[0] != 0

	default:
		return nil, fmt.Errorf("unsupported bool variable byte size %v", size)
	}

	// TODO: what should the string parameter be?
	boolTypeRecord := trace_record.NewSimpleTypeRecord(trace_record.INT_TYPE_KIND, "Boolean")
	typeId := record.RegisterTypeWithNewId(typ.Name, boolTypeRecord)

	return trace_record.BoolValue(boolVal, typeId), nil
}

func bytesToStruct(rawBytes []byte, typ *dwarf.StructType, m *wasm.ModuleInstance) (trace_record.ValueRecord, error) {
	values := make([]trace_record.ValueRecord, 0)

	record := m.Record

	for _, field := range typ.Field {
		offset := field.ByteOffset
		size := field.Type.Size()
		res, err := bytesToValueRecord(rawBytes[offset:offset+size], field.Type, m)
		if err != nil {
			return nil, err
		}

		values = append(values, res)

	}

	// TODO: what should the string parameter be?
	structTypeRecord := trace_record.NewSimpleTypeRecord(trace_record.STRUCT_TYPE_KIND, "Struct")
	typeId := record.RegisterTypeWithNewId(typ.Name, structTypeRecord)

	return trace_record.StructValue(values, typeId), nil
}

func bytesToPointer(rawBytes []byte, typ *dwarf.PtrType, m *wasm.ModuleInstance) (trace_record.ValueRecord, error) {

	dereferencedType := typ.Type

	record := m.Record

	mem := m.Memory()

	addr := binary.LittleEndian.Uint32(rawBytes)

	// TODO: Handle errors
	dereferencedRawBytes, _ := mem.Read(addr, uint32(dereferencedType.Size()))

	// TODO: Handle errors
	dereferencedValueRecord, _ := bytesToValueRecord(dereferencedRawBytes, dereferencedType, m)

	// TODO: what should the string parameter be?
	// TODO: Define PTR_TYPE_KIND in trace_record
	pointerTypeRecord := trace_record.NewSimpleTypeRecord(trace_record.INT_TYPE_KIND, "Pointer")
	typeId := record.RegisterTypeWithNewId(typ.Name, pointerTypeRecord)

	return trace_record.ReferenceValue(dereferencedValueRecord, false, typeId), nil

}
