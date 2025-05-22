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
	memAddr := uint32(locals[functionRecord.FrameBaseIndex] + v.Offset)
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

	switch t := typ.(type) {
	case *dwarf.IntType:
		val, err = bytesToInt(rawBytes, t, m)

	case *dwarf.UintType:
		val, err = bytesToUint(rawBytes, t, m)

	case *dwarf.BoolType:
		val, err = bytesToBool(rawBytes, t, m)

	case *dwarf.FloatType:
		val, err = bytesToFloat(rawBytes, t, m)

	case *dwarf.StructType:
		// TODO: make these language specific
		typeStr := typ.String()
		if typeStr == "struct &str" {
			val, err = bytesToStringRust(rawBytes, t, m)
		} else if strings.HasPrefix(typeStr, "struct &[") && strings.HasSuffix(typeStr, "]") {
			val, err = bytesToSliceRust(rawBytes, t, m)
		} else {

			val, err = bytesToStruct(rawBytes, t, m)
		}

	case *dwarf.PtrType:
		val, err = bytesToPointer(rawBytes, t, m)

	case *dwarf.ArrayType:
		val, err = bytesToArray(rawBytes, t, m)

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

	typeName := typ.Name

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
	// intTypeRecord := trace_record.NewSimpleTypeRecord(trace_record.INT_TYPE_KIND, "Int")
	// typeId := record.RegisterTypeWithNewId(typ.Name, intTypeRecord)

	// NOTE: We call this only from bytesToValueRecord,
	// which ensures that this type has had a TypeId assigned before this function was called
	typeId, seen := m.TypesIndex[typ.String()]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.INT_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.IntValue(intVal, typeId), nil
}

func bytesToUint(rawBytes []byte, typ *dwarf.UintType, m *wasm.ModuleInstance) (trace_record.ValueRecord, error) {
	size := typ.ByteSize
	var intVal uint64

	typeName := typ.Name

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

	// NOTE: We call this only from bytesToValueRecord,
	// which ensures that this type has had a TypeId assigned before this function was called
	typeId, seen := m.TypesIndex[typ.String()]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.INT_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	// TODO: discuss int64 uint64 stuff?
	return trace_record.IntValue(int64(intVal), typeId), nil
}

func bytesToBool(rawBytes []byte, typ *dwarf.BoolType, m *wasm.ModuleInstance) (trace_record.ValueRecord, error) {
	size := typ.ByteSize
	var boolVal bool

	typeName := typ.String()

	switch size {
	case 1:
		boolVal = rawBytes[0] != 0

	default:
		return nil, fmt.Errorf("unsupported bool variable byte size %v", size)
	}

	// NOTE: We call this only from bytesToValueRecord,
	// which ensures that this type has had a TypeId assigned before this function was called
	typeId, seen := m.TypesIndex[typ.String()]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.BOOL_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.BoolValue(boolVal, typeId), nil
}

func bytesToFloat(rawBytes []byte, typ *dwarf.FloatType, m *wasm.ModuleInstance) (trace_record.ValueRecord, error) {
	size := typ.ByteSize
	var floatVal float64

	typeName := typ.String()

	switch size {
	case 4:
		floatVal = float64(math.Float32frombits(binary.LittleEndian.Uint32(rawBytes)))

	case 8:
		floatVal = math.Float64frombits(binary.LittleEndian.Uint64(rawBytes))

	default:
		return nil, fmt.Errorf("unsupported float variable byte size %v", size)
	}

	// NOTE: We call this only from bytesToValueRecord,
	// which ensures that this type has had a TypeId assigned before this function was called
	typeId, seen := m.TypesIndex[typ.String()]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.FLOAT_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.FloatValue(floatVal, typeId), nil
}

// TODO: Finish
func bytesToStruct(rawBytes []byte, typ *dwarf.StructType, m *wasm.ModuleInstance) (trace_record.ValueRecord, error) {
	values := make([]trace_record.ValueRecord, 0)

	typeName := typ.String()

	types := make([]trace_record.FieldTypeRecord, 0)

	for _, field := range typ.Field {
		offset := field.ByteOffset
		size := field.Type.Size()
		fieldName := field.Name

		res, fieldTypeId, err := bytesToValueRecord(rawBytes[offset:offset+size], field.Type, m)

		fieldTypeRecord := trace_record.NewFieldTypeRecord(fieldName, fieldTypeId)
		types = append(types, fieldTypeRecord)

		if err != nil {
			return nil, err
		}

		values = append(values, res)

	}

	// NOTE: We call this only from bytesToValueRecord,
	// which ensures that this type has had a TypeId assigned before this function was called
	typeId, seen := m.TypesIndex[typ.String()]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeSpecificInfo := trace_record.NewStructTypeInfo(types)

		typeRecord := trace_record.NewTypeRecord(trace_record.FLOAT_TYPE_KIND, typeName, typeSpecificInfo)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.StructValue(values, typeId), nil
}

func bytesToPointer(rawBytes []byte, typ *dwarf.PtrType, m *wasm.ModuleInstance) (trace_record.ValueRecord, error) {

	dereferencedType := typ.Type

	mem := m.Memory()

	typeName := typ.String()

	addr := binary.LittleEndian.Uint32(rawBytes)

	// TODO: Handle errors
	dereferencedRawBytes, _ := mem.Read(addr, uint32(dereferencedType.Size()))

	// TODO: Handle errors
	// TODO: Construct array Type info, DO NOT ignore it
	dereferencedValueRecord, dereferencedTypeId, _ := bytesToValueRecord(dereferencedRawBytes, dereferencedType, m)

	// NOTE: We call this only from bytesToValueRecord,
	// which ensures that this type has had a TypeId assigned before this function was called
	typeId, seen := m.TypesIndex[typ.String()]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeSpecificInfo := trace_record.NewPointerTypeInfo(dereferencedTypeId)

		typeRecord := trace_record.NewTypeRecord(trace_record.FLOAT_TYPE_KIND, typeName, typeSpecificInfo)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	// TODO: Record pointer Type info

	return trace_record.ReferenceValue(dereferencedValueRecord, addr, false, typeId), nil

}

func bytesToArray(rawBytes []byte, typ *dwarf.ArrayType, m *wasm.ModuleInstance) (trace_record.ValueRecord, error) {

	elemSize := typ.Type.Size()

	arrayLen := typ.Count

	typeName := typ.String()

	elems := make([]trace_record.ValueRecord, 0)

	for i := 0; i < int(arrayLen)-1; i++ {

		// TODO: Construct array Type info, DO NOT ignore it
		elem, _, _ := bytesToValueRecord(rawBytes[i*int(elemSize):(i+1)*int(elemSize)], typ.Type, m)
		elems = append(elems, elem)

	}

	// TODO: Record array Type info
	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.ARRAY_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.SequenceValue(elems, false, typeId), nil

}
