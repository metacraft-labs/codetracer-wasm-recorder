package interpreter

import (
	"debug/dwarf"
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"github.com/tetratelabs/wazero/internal/tracetypes"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/wasmdebug"
)

func readVariable(m *wasm.ModuleInstance, v wasmdebug.VariableRecord, functionRecord wasmdebug.FunctionRecord, locals []uint64) (tracetypes.ValueRecord, error) {
	fb := functionRecord.FrameBase

	var memAddr uint32

	// Of the variable has a `Direct` location type, it takes precedence over all
	// all other types of location identification
	if v.Location.Typ == wasmdebug.LocationTypeDirectLocal {
		memAddr = uint32(locals[v.Location.Index])
	} else if fb.Typ == wasmdebug.LocationTypeLocal {
		memAddr = uint32(locals[fb.Index] + uint64(v.Location.Index))
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

func bytesToValueRecord(rawBytes []byte, typ dwarf.Type, m *wasm.ModuleInstance) (val tracetypes.ValueRecord, typeId tracetypes.TypeId, err error) {
	switch t := typ.(type) {
	case *dwarf.IntType:
		val, typeId, err = bytesToInt(rawBytes, t, m)

	case *dwarf.UintType:
		// TODO: make these language specific
		typeStr := typ.String()
		if typeStr == "()" {
			val, typeId, err = bytesToVoidptr(rawBytes, t, m)
		} else {
			val, typeId, err = bytesToUint(rawBytes, t, m)
		}

	case *dwarf.BoolType:
		val, typeId, err = bytesToBool(rawBytes, t, m)

	case *dwarf.FloatType:
		val, typeId, err = bytesToFloat(rawBytes, t, m)

	case *dwarf.StructType:
		// TODO: make these language specific
		typeStr := typ.String()
		if typeStr == "struct &str" {
			val, typeId, err = bytesToStringRust(rawBytes, t, m)
		} else if strings.HasPrefix(typeStr, "struct (") && strings.HasSuffix(typeStr, ")") {
			val, typeId, err = bytesToTupleRust(rawBytes, t, m)
		} else if strings.HasPrefix(typeStr, "struct &[") && strings.HasSuffix(typeStr, "]") {
			val, typeId, err = bytesToSliceRust(rawBytes, t, m)
		} else if strings.HasPrefix(typeStr, "struct Vec<") && strings.HasSuffix(typeStr, ">") {
			val, typeId, err = bytesToVecRust(rawBytes, t, m)
		} else if typeStr == "struct Address" {
			val, typeId, err = bytesToAddressRust(rawBytes, t, m)
		} else if strings.HasPrefix(typeStr, "struct Uint<") && strings.HasSuffix(typeStr, ">") {
			val, typeId, err = bytesToRuintRust(rawBytes, t, m)
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
		val = tracetypes.NilValue()
	}

	return
}

const INVALID_TYPE_ID = tracetypes.TypeId(0xffffffffffffffff)

func bytesToInt(rawBytes []byte, typ *dwarf.IntType, m *wasm.ModuleInstance) (tracetypes.ValueRecord, tracetypes.TypeId, error) {
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
		return nil, INVALID_TYPE_ID, fmt.Errorf("unsupported int variable byte size %v", size)
	}

	// TODO: what should the string parameter be?
	// intTypeRecord := tracetypes.NewSimpleTypeRecord(tracetypes.INT_TYPE_KIND, "Int")
	// typeId := record.RegisterTypeWithNewId(typ.Name, intTypeRecord)

	typeName := typ.String()

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = tracetypes.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := tracetypes.NewSimpleTypeRecord(tracetypes.INT_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return tracetypes.IntValue(intVal, typeId), typeId, nil
}

func bytesToUint(rawBytes []byte, typ *dwarf.UintType, m *wasm.ModuleInstance) (tracetypes.ValueRecord, tracetypes.TypeId, error) {
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
		return nil, INVALID_TYPE_ID, fmt.Errorf("unsupported uint variable byte size %v", size)
	}

	typeName := typ.String()

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = tracetypes.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := tracetypes.NewSimpleTypeRecord(tracetypes.INT_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	// TODO: discuss int64 uint64 stuff?
	return tracetypes.IntValue(int64(intVal), typeId), typeId, nil
}

func bytesToBool(rawBytes []byte, typ *dwarf.BoolType, m *wasm.ModuleInstance) (tracetypes.ValueRecord, tracetypes.TypeId, error) {
	size := typ.ByteSize
	var boolVal bool

	switch size {
	case 1:
		boolVal = rawBytes[0] != 0

	default:
		return nil, INVALID_TYPE_ID, fmt.Errorf("unsupported bool variable byte size %v", size)
	}

	typeName := typ.String()

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = tracetypes.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := tracetypes.NewSimpleTypeRecord(tracetypes.BOOL_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return tracetypes.BoolValue(boolVal, typeId), typeId, nil
}

func bytesToFloat(rawBytes []byte, typ *dwarf.FloatType, m *wasm.ModuleInstance) (tracetypes.ValueRecord, tracetypes.TypeId, error) {
	size := typ.ByteSize
	var floatVal float64

	switch size {
	case 4:
		floatVal = float64(math.Float32frombits(binary.LittleEndian.Uint32(rawBytes)))

	case 8:
		floatVal = math.Float64frombits(binary.LittleEndian.Uint64(rawBytes))

	default:
		return nil, INVALID_TYPE_ID, fmt.Errorf("unsupported float variable byte size %v", size)
	}

	typeName := typ.String()

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = tracetypes.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := tracetypes.NewSimpleTypeRecord(tracetypes.FLOAT_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return tracetypes.FloatValue(floatVal, typeId), typeId, nil
}

// TODO: Finish
func bytesToStruct(rawBytes []byte, typ *dwarf.StructType, m *wasm.ModuleInstance) (tracetypes.ValueRecord, tracetypes.TypeId, error) {
	values := make([]tracetypes.ValueRecord, 0)

	types := make([]tracetypes.FieldTypeRecord, 0)

	for _, field := range typ.Field {
		offset := field.ByteOffset
		size := field.Type.Size()
		fieldName := field.Name

		res, fieldTypeId, err := bytesToValueRecord(rawBytes[offset:offset+size], field.Type, m)

		fieldTypeRecord := tracetypes.NewFieldTypeRecord(fieldName, fieldTypeId)
		types = append(types, fieldTypeRecord)

		if err != nil {
			return nil, INVALID_TYPE_ID, err
		}

		values = append(values, res)

	}

	typeName := typ.String()

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = tracetypes.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeSpecificInfo := tracetypes.NewStructTypeInfo(types)

		typeRecord := tracetypes.NewTypeRecord(tracetypes.STRUCT_TYPE_KIND, typeName, typeSpecificInfo)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return tracetypes.StructValue(values, typeId), typeId, nil
}

func bytesToPointer(rawBytes []byte, typ *dwarf.PtrType, m *wasm.ModuleInstance) (tracetypes.ValueRecord, tracetypes.TypeId, error) {

	dereferencedType := typ.Type

	mem := m.Memory()

	addr := binary.LittleEndian.Uint32(rawBytes)

	// TODO: Handle errors
	dereferencedRawBytes, ok := mem.Read(addr, uint32(dereferencedType.Size()))

	if !ok {
		return nil, INVALID_TYPE_ID, fmt.Errorf("invalid memory access")
	}

	// NOTE: What do we do when the dereferencedType's size is 0 ?

	// TODO: Handle errors
	dereferencedValueRecord, dereferencedTypeId, _ := bytesToValueRecord(dereferencedRawBytes, dereferencedType, m)

	if dereferencedValueRecord == nil || dereferencedType.Size() == 0 {
		dereferencedValueRecord = tracetypes.NilValue()
	}

	typeName := typ.String()

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = tracetypes.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeSpecificInfo := tracetypes.NewPointerTypeInfo(dereferencedTypeId)

		typeRecord := tracetypes.NewTypeRecord(tracetypes.POINTER_TYPE_KIND, typeName, typeSpecificInfo)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	// TODO: Record pointer Type info

	return tracetypes.ReferenceValue(dereferencedValueRecord, uint64(addr), false, typeId), typeId, nil

}

func bytesToArray(rawBytes []byte, typ *dwarf.ArrayType, m *wasm.ModuleInstance) (tracetypes.ValueRecord, tracetypes.TypeId, error) {

	elemSize := typ.Type.Size()

	arrayLen := typ.Count

	elems := make([]tracetypes.ValueRecord, 0)

	for i := 0; i < int(arrayLen); i++ {

		// TODO: Construct array Type info, DO NOT ignore it
		elem, _, _ := bytesToValueRecord(rawBytes[i*int(elemSize):(i+1)*int(elemSize)], typ.Type, m)
		elems = append(elems, elem)

	}

	typeName := typ.String()

	// TODO: Record array Type info
	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = tracetypes.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := tracetypes.NewSimpleTypeRecord(tracetypes.ARRAY_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return tracetypes.SequenceValue(elems, false, typeId), typeId, nil

}
