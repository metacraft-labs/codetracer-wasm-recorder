package interpreter

import (
	"debug/dwarf"
	"fmt"

	"github.com/metacraft-labs/trace_record"
	"github.com/tetratelabs/wazero/internal/wasm"
)

func bytesToStringRust(rawBytes []byte, typ *dwarf.StructType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {

	typeName := typ.String()

	mem := m.Memory()

	data, _, _ := bytesToStruct(rawBytes, typ, m)

	val, ok := data.(trace_record.StructValueRecord)

	if !ok {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a string slice")
	}

	data_ptr_field, err := val.Fields[0].(trace_record.ReferenceValueRecord)

	if !err {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a string slice")
	}

	addr := data_ptr_field.Address

	len_field, ok := val.Fields[1].(trace_record.IntValueRecord)

	if !ok {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a string slice")
	}

	length := len_field.I

	str := ""

	for i := 0; i < int(length); i++ {
		data, _ := mem.Read(addr+uint32(i), 1)
		str += string(data[0])
	}

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.STRING_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.StringValue(str, typeId), INVALID_TYPE_ID, nil
}

func bytesToTupleRust(rawBytes []byte, typ *dwarf.StructType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {

	typeName := typ.String()

	elemSize := uint32(typ.Field[0].Type.Size())

	fieldTypes := typ.Field

	tupleLength := uint32(len(fieldTypes))

	elems := make([]trace_record.ValueRecord, 0)

	for i := uint32(0); i < tupleLength; i++ {
		tupleElem, _, _ := bytesToValueRecord(rawBytes[i*elemSize:(i+1)*elemSize], fieldTypes[i].Type, m)

		elems = append(elems, tupleElem)
	}

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.SLICE_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.TupleValue(elems, typeId), INVALID_TYPE_ID, nil
}

func bytesToSliceRust(rawBytes []byte, typ *dwarf.StructType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {

	typeName := typ.String()

	mem := m.Memory()

	rawStruct, _, err := bytesToStruct(rawBytes, typ, m)
	if err != nil {
		return nil, INVALID_TYPE_ID, err
	}

	fields := rawStruct.(trace_record.StructValueRecord).Fields

	if len(fields) != 2 {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a slice")
	}

	var addr uint32
	if addrRecord, ok := fields[0].(trace_record.ReferenceValueRecord); ok {
		addr = addrRecord.Address
	} else {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a slice")
	}

	var length uint32
	if lenRecord, ok := fields[1].(trace_record.IntValueRecord); ok {
		length = uint32(lenRecord.I)
	} else {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a slice")
	}

	var elemType dwarf.Type
	var elemSize uint32
	if ptrTyp, ok := typ.Field[0].Type.(*dwarf.PtrType); ok {
		elemType = ptrTyp.Type
		elemSize = uint32(ptrTyp.Type.Common().ByteSize)
	} else {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a slice")
	}

	// TODO: what type kind?
	// TODO: what should the string parameter be?

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.SLICE_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	elems := make([]trace_record.ValueRecord, 0)
	for i := uint32(0); i < length; i++ {
		elemBytes, ok := mem.Read(addr+i*elemSize, elemSize)
		if !ok {
			return trace_record.SequenceValue(elems, true, typeId), INVALID_TYPE_ID, fmt.Errorf("invalid memory access")
		}

		// TODO: Construct array Type info, DO NOT ignore it
		elem, _, err := bytesToValueRecord(elemBytes, elemType, m)
		if err != nil {
			return trace_record.SequenceValue(elems, true, typeId), INVALID_TYPE_ID, err
		}

		elems = append(elems, elem)
	}

	return trace_record.SequenceValue(elems, true, typeId), INVALID_TYPE_ID, nil

}

func bytesToVecRust(rawBytes []byte, typ *dwarf.StructType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {
	// Vec<T> in Rust stdlib is essentially { buf: RawVec<T>, len: usize }.
	// The element type is stored in the TypeParamMap of the PCRecord.

	mem := m.Memory()
	typeName := typ.String()

	// Resolve the element type via the TypeParamMap. The map is guaranteed
	// to contain the "T" parameter.
	var elemType dwarf.Type
	if m.Source != nil {
		if mp, ok := m.Source.PCRecord.TypeParamMap[typeName]; ok {
			elemType = mp["T"]
		}
	}
	if elemType == nil {
		return nil, INVALID_TYPE_ID, fmt.Errorf("could not resolve vec element type")
	}

	// Decode the Vec struct itself to obtain the data pointer and length.
	rawStruct, _, err := bytesToStruct(rawBytes, typ, m)
	if err != nil {
		return nil, INVALID_TYPE_ID, err
	}

	fields := rawStruct.(trace_record.StructValueRecord).Fields
	if len(fields) != 2 {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a vec")
	}

	// First field is RawVec<T> which holds the pointer in its first subfield
	// (Unique<T>).
	rawVecField, ok := fields[0].(trace_record.StructValueRecord)
	if !ok {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a vec")
	}

	rawVecFields := rawVecField.Fields
	if len(rawVecFields) < 1 {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a vec")
	}

	rawVecInnerField, ok := rawVecFields[0].(trace_record.StructValueRecord)
	if !ok {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a vec")
	}

	if len(rawVecInnerField.Fields) < 1 {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a vec")
	}

	ptrUniqField, ok := rawVecInnerField.Fields[0].(trace_record.StructValueRecord)
	if !ok {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a vec")
	}

	ptrNonNullField, ok := ptrUniqField.Fields[0].(trace_record.StructValueRecord)
	if !ok {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a vec")
	}

	ptrField, ok := ptrNonNullField.Fields[0].(trace_record.ReferenceValueRecord)
	if !ok {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a vec")
	}

	addr := ptrField.Address

	// Second field of Vec is the current length.
	lenRecord, ok := fields[1].(trace_record.IntValueRecord)
	if !ok {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a vec")
	}
	length := uint32(lenRecord.I)

	elemSize := uint32(elemType.Common().ByteSize)

	// Register Vec type in trace record if needed.
	typeId, seen := m.TypesIndex[typeName]
	if !seen {
		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]
		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.SLICE_TYPE_KIND, typeName)
		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	elems := make([]trace_record.ValueRecord, 0)
	for i := uint32(0); i < length; i++ {
		elemBytes, ok := mem.Read(addr+i*elemSize, elemSize)
		if !ok {
			return trace_record.SequenceValue(elems, true, typeId), INVALID_TYPE_ID, fmt.Errorf("invalid memory access")
		}

		elem, _, err := bytesToValueRecord(elemBytes, elemType, m)
		if err != nil {
			return trace_record.SequenceValue(elems, true, typeId), INVALID_TYPE_ID, err
		}

		elems = append(elems, elem)
	}

	return trace_record.SequenceValue(elems, true, typeId), INVALID_TYPE_ID, nil
	//	// Vec<T> in Rust stdlib is essentially { buf: RawVec<T>, len: usize }.
	//	// RawVec<T> is { ptr: Unique<T>, cap: usize, alloc: Global } where Unique<T>
	//	// is repr(transparent) over *const T. Therefore on wasm32 the layout is:
	//	// [0:4] data pointer, [4:8] capacity, [8:12] length.
	//
	//	mem := m.Memory()
	//	typeName := typ.String()
	//
	//	// Determine the element type via the PCRecord's TypeParamMap if available.
	//	var elemType dwarf.Type
	//	if m.Source != nil {
	//		if mp, ok := m.Source.PCRecord.TypeParamMap[typeName]; ok {
	//			if t, ok := mp["T"]; ok {
	//				elemType = t
	//			}
	//		}
	//	}
	//	// Fallback: attempt to extract from the first field if not found.
	//	// if elemType == nil && len(typ.Field) > 0 {
	//	// 	if rawVec, ok := typ.Field[0].Type.(*dwarf.StructType); ok {
	//	// 		if len(rawVec.Field) > 0 {
	//	// 			if unique, ok := rawVec.Field[0].Type.(*dwarf.StructType); ok {
	//	// 				if len(unique.Field) > 0 {
	//	// 					if ptr, ok := unique.Field[0].Type.(*dwarf.PtrType); ok {
	//	// 						elemType = ptr.Type
	//	// 					}
	//	// 				}
	//	// 			}
	//	// 		}
	//	// 	}
	//	// }
	//	if elemType == nil {
	//		return nil, INVALID_TYPE_ID, fmt.Errorf("could not resolve vec element type")
	//	}
	//
	//	fmt.Printf("ELEM TYPE: %#v\n", elemType)
	//
	//	addr := binary.LittleEndian.Uint32(rawBytes[0:4])
	//	length := binary.LittleEndian.Uint32(rawBytes[8:12])
	//
	//	elemSize := uint32(elemType.Size())
	//
	//	// Register Vec type in trace record if needed.
	//	typeId, seen := m.TypesIndex[typeName]
	//	if !seen {
	//		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
	//		typeId = m.TypesIndex[typeName]
	//		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.SLICE_TYPE_KIND, typeName)
	//		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	//	}
	//
	//	elems := make([]trace_record.ValueRecord, 0)
	//	for i := uint32(0); i < length; i++ {
	//		elemBytes, ok := mem.Read(addr+i*elemSize, elemSize)
	//		if !ok {
	//			return trace_record.SequenceValue(elems, true, typeId), INVALID_TYPE_ID, fmt.Errorf("invalid memory access")
	//		}
	//
	//		elem, _, err := bytesToValueRecord(elemBytes, elemType, m)
	//		if err != nil {
	//			return trace_record.SequenceValue(elems, true, typeId), INVALID_TYPE_ID, err
	//		}
	//
	//		elems = append(elems, elem)
	//	}
	//
	//	return trace_record.SequenceValue(elems, true, typeId), INVALID_TYPE_ID, nil

}

// TODO: maybe this is not Rust specific?
func bytesToVoidptr(rawBytes []byte, typ *dwarf.UintType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {
	typeName := typ.String()

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.SLICE_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.NilValue(), typeId, nil
}
