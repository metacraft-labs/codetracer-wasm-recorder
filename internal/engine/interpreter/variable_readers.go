package interpreter

import (
	"debug/dwarf"
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"github.com/metacraft-labs/trace_record"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/wasmdebug"
)

func readVariable(m *wasm.ModuleInstance, v wasmdebug.VariableRecord, functionRecord wasmdebug.FunctionRecord, locals []uint64) (trace_record.ValueRecord, error) {
	fb := functionRecord.FrameBase

	var memAddr uint32

	if fb.Typ == wasmdebug.LocationTypeLocal {
		memAddr = uint32(locals[fb.Index] + v.Offset)
	} else if fb.Typ == wasmdebug.LocationTypeGlobal {
		memAddr = uint32(m.Global(int(fb.Index)).Get())
	} else if fb.Typ == wasmdebug.OperandStack {
		// TODO: support
		return nil, fmt.Errorf("unsupported location type")
	} else {
		return nil, fmt.Errorf("invalid location type")
	}
	memSize := uint32(v.Type.Size())

	mem := m.Memory()

	rawBytes, ok := mem.Read(memAddr, memSize)
	if !ok {
		return nil, fmt.Errorf("out of range memory access")
	}

	valueRecord, _, err := bytesToValueRecord(rawBytes, v.Type, m)

	return valueRecord, err
}

func bytesToValueRecord(rawBytes []byte, typ dwarf.Type, m *wasm.ModuleInstance) (val trace_record.ValueRecord, typeId trace_record.TypeId, err error) {
	fmt.Printf("PARSING BYTES FOR TYPE %v. SIZE IS %v AND RAW BYTES ARE %v\n", typ.String(), typ.Size(), len(rawBytes))

	switch t := typ.(type) {
	case *dwarf.IntType:
		val, typeId, err = bytesToInt(rawBytes, t, m)

	case *dwarf.UintType:
		val, typeId, err = bytesToUint(rawBytes, t, m)

	case *dwarf.BoolType:
		val, typeId, err = bytesToBool(rawBytes, t, m)

	case *dwarf.FloatType:
		val, typeId, err = bytesToFloat(rawBytes, t, m)

	case *dwarf.StructType:
		// TODO: make these language specific
		typeStr := typ.String()
		if typeStr == "struct &str" {
			val, typeId, err = bytesToStringRust(rawBytes, t, m)
		} else if strings.HasPrefix(typeStr, "struct &[") && strings.HasSuffix(typeStr, "]") {
			val, typeId, err = bytesToSliceRust(rawBytes, t, m)
		} else {

			val, typeId, err = bytesToStruct(rawBytes, t, m)
		}

	case *dwarf.PtrType:
		val, typeId, err = bytesToPointer(rawBytes, t, m)

	case *dwarf.ArrayType:
		val, typeId, err = bytesToArray(rawBytes, t, m)

	default:
		fmt.Printf("WE HAVE SOMETHING ELSE: %T %#v\n", t, t)
		// TODO
		val = trace_record.NilValue()
	}

	return
}

func bytesToInt(rawBytes []byte, typ *dwarf.IntType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {
	size := typ.ByteSize
	var intVal int64

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
		return nil, trace_record.TypeId(0xffffffffffffffff), fmt.Errorf("unsupported int variable byte size %v", size)
	}

	// TODO: what should the string parameter be?
	// intTypeRecord := trace_record.NewSimpleTypeRecord(trace_record.INT_TYPE_KIND, "Int")
	// typeId := record.RegisterTypeWithNewId(typ.Name, intTypeRecord)

	typeName := typ.String()

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.INT_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.IntValue(intVal, typeId), typeId, nil
}

func bytesToUint(rawBytes []byte, typ *dwarf.UintType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {
	size := typ.ByteSize
	var intVal uint64

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
		return nil, trace_record.TypeId(0xffffffffffffffff), fmt.Errorf("unsupported uint variable byte size %v", size)
	}

	typeName := typ.String()

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.INT_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	// TODO: discuss int64 uint64 stuff?
	return trace_record.IntValue(int64(intVal), typeId), typeId, nil
}

func bytesToBool(rawBytes []byte, typ *dwarf.BoolType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {
	size := typ.ByteSize
	var boolVal bool

	switch size {
	case 1:
		boolVal = rawBytes[0] != 0

	default:
		return nil, trace_record.TypeId(0xffffffffffffffff), fmt.Errorf("unsupported bool variable byte size %v", size)
	}

	typeName := typ.String()

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.BOOL_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.BoolValue(boolVal, typeId), typeId, nil
}

func bytesToFloat(rawBytes []byte, typ *dwarf.FloatType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {
	size := typ.ByteSize
	var floatVal float64

	switch size {
	case 4:
		floatVal = float64(math.Float32frombits(binary.LittleEndian.Uint32(rawBytes)))

	case 8:
		floatVal = math.Float64frombits(binary.LittleEndian.Uint64(rawBytes))

	default:
		return nil, trace_record.TypeId(0xffffffffffffffff), fmt.Errorf("unsupported float variable byte size %v", size)
	}

	typeName := typ.String()

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.FLOAT_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.FloatValue(floatVal, typeId), typeId, nil
}

// TODO: Finish
func bytesToStruct(rawBytes []byte, typ *dwarf.StructType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {
	values := make([]trace_record.ValueRecord, 0)

	types := make([]trace_record.FieldTypeRecord, 0)

	for _, field := range typ.Field {
		offset := field.ByteOffset
		size := field.Type.Size()
		fieldName := field.Name

		res, fieldTypeId, err := bytesToValueRecord(rawBytes[offset:offset+size], field.Type, m)

		fieldTypeRecord := trace_record.NewFieldTypeRecord(fieldName, fieldTypeId)
		types = append(types, fieldTypeRecord)

		if err != nil {
			return nil, trace_record.TypeId(0xffffffffffffffff), err
		}

		values = append(values, res)

	}

	typeName := typ.String()

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeSpecificInfo := trace_record.NewStructTypeInfo(types)

		typeRecord := trace_record.NewTypeRecord(trace_record.STRUCT_TYPE_KIND, typeName, typeSpecificInfo)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.StructValue(values, typeId), typeId, nil
}

func bytesToPointer(rawBytes []byte, typ *dwarf.PtrType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {

	dereferencedType := typ.Type

	mem := m.Memory()

	addr := binary.LittleEndian.Uint32(rawBytes)

	// TODO: Handle errors
	dereferencedRawBytes, _ := mem.Read(addr, uint32(dereferencedType.Size()))

	// TODO: Handle errors
	// TODO: Construct array Type info, DO NOT ignore it
	dereferencedValueRecord, dereferencedTypeId, _ := bytesToValueRecord(dereferencedRawBytes, dereferencedType, m)

	typeName := typ.String()

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeSpecificInfo := trace_record.NewPointerTypeInfo(dereferencedTypeId)

		typeRecord := trace_record.NewTypeRecord(trace_record.POINTER_TYPE_KIND, typeName, typeSpecificInfo)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	// TODO: Record pointer Type info

	return trace_record.ReferenceValue(dereferencedValueRecord, addr, false, typeId), typeId, nil

}

func bytesToArray(rawBytes []byte, typ *dwarf.ArrayType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {

	elemSize := typ.Type.Size()

	arrayLen := typ.Count

	elems := make([]trace_record.ValueRecord, 0)

	for i := 0; i < int(arrayLen)-1; i++ {

		// TODO: Construct array Type info, DO NOT ignore it
		elem, _, _ := bytesToValueRecord(rawBytes[i*int(elemSize):(i+1)*int(elemSize)], typ.Type, m)
		elems = append(elems, elem)

	}

	typeName := typ.String()

	// TODO: Record array Type info
	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.ARRAY_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.SequenceValue(elems, false, typeId), typeId, nil

}
