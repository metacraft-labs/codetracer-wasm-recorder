package interpreter

import (
	"debug/dwarf"
	"encoding/binary"
	"fmt"

	"github.com/metacraft-labs/trace_record"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/wasmdebug"
)

func readVariable(m *wasm.ModuleInstance, v wasmdebug.VariableRecord, functionRecord wasmdebug.FunctionRecord, locals []uint64) (val trace_record.ValueRecord, err error) {
	memAddr := uint32(locals[functionRecord.FrameBaseIndex] + v.Offset)
	switch t := v.Type.(type) {
	case *dwarf.IntType:
		val, err = readIntVariable(uint64(memAddr), m, t)
	case *dwarf.StructType:
		val, err = readStructVariable(uint64(memAddr), v, m)

	default:
		fmt.Printf("WE HAVE SOMETHING ELSE: %T\n", t)
		// TODO
		val = trace_record.NilValue()
	}

	return
}

func readIntVariable(
	offset uint64,
	m *wasm.ModuleInstance,
	intType *dwarf.IntType) (trace_record.ValueRecord, error) {

	mem := m.Memory()

	rawVal, _ := mem.Read(uint32(offset), uint32(intType.ByteSize))

	val := binary.LittleEndian.Uint32(rawVal)

	// fmt.Printf("local INT: %s has decimal value: %d\n", v.Name, val)

	intTypeRecord := trace_record.NewSimpleTypeRecord(trace_record.INT_TYPE_KIND, "Int")
	typeId := m.Record.RegisterTypeWithNewId("Int", intTypeRecord)
	// m.Record.RegisterVariable(v.Name, trace_record.IntValue(int64(val), typeId))

	return trace_record.IntValue(int64(val), typeId), nil

}

func doReadStructVariable(currOffset uint64, currType *dwarf.Type, m *wasm.ModuleInstance) ([]trace_record.ValueRecord, error) {

	values := make([]trace_record.ValueRecord, 0)
	switch t := (*currType).(type) {
	case *dwarf.StructType:
		structFields := make([]trace_record.ValueRecord, 0)
		for _, field := range t.Field {
			res, err := doReadStructVariable(currOffset+uint64(field.ByteOffset), &field.Type, m)
			if err == nil {
				structFields = append(structFields, res...)
			}
		}

		structTypeRecord := trace_record.NewSimpleTypeRecord(trace_record.STRUCT_TYPE_KIND, t.Common().Name)
		typeId := m.Record.RegisterTypeWithNewId(t.Name, structTypeRecord)

		structValueRecord := trace_record.StructValue(structFields, typeId)

		values = append(values, structValueRecord)
	case *dwarf.IntType:
		intValue, err := readIntVariable(currOffset, m, t)
		if err == nil {
			values = append(values, intValue)
		}

	default:
		return values, fmt.Errorf("unsupported Variable Type encountered in struct")
	}

	return values, nil
}

func readStructVariable(currOffset uint64, v wasmdebug.VariableRecord, m *wasm.ModuleInstance) (trace_record.ValueRecord, error) {
	fields, err := doReadStructVariable(currOffset, &v.Type, m)
	if err != nil {
		return nil, err
	}

	structTypeRecord := trace_record.NewSimpleTypeRecord(trace_record.STRUCT_TYPE_KIND, v.Type.Common().Name)
	typeId := m.Record.RegisterTypeWithNewId(v.Name, structTypeRecord)

	return trace_record.StructValue(fields, typeId), nil

}

// func traceStructVariable(
// 	frameBase uint64,
// 	v *wasmdebug.VariableRecord,
// 	locals [10000]uint64,
// 	m *wasm.ModuleInstance,
// 	structType *dwarf.StructType) error {
//
// 	mem := m.Memory()
//
// 	rawVal, _ := mem.Read(uint32(frameBase+v.Offset), uint32(structType.ByteSize))
//
// 	val := binary.LittleEndian.Uint32(rawVal)
//
// 	fmt.Printf("local INT: %s has decimal value: %d\n", v.Name, val)
//
// 	structTypeRecord := trace_record.NewSimpleTypeRecord(trace_record.STRUCT_TYPE_KIND, "Struct")
// 	typeId := m.Record.RegisterTypeWithNewId("Struct", structTypeRecord)
// 	records := []trace_record.ValueRecord{}
// 	m.Record.RegisterVariable(v.Name, trace_record.StructValue(records, typeId))
//
// 	return nil
//
// }
//
