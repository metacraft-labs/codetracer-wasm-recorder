package interpreter

import (
	"debug/dwarf"
	"encoding/binary"
	"fmt"
	"slices"

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

	return trace_record.StringValue(str, typeId), typeId, nil
}

func bytesToTupleRust(rawBytes []byte, typ *dwarf.StructType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {
	typeName := typ.String()

	elems := make([]trace_record.ValueRecord, 0)

	for _, field := range typ.Field {
		startByte := field.ByteOffset
		endByte := startByte + field.Type.Size()

		tupleElem, _, _ := bytesToValueRecord(rawBytes[startByte:endByte], field.Type, m)

		elems = append(elems, tupleElem)
	}

	typeId, seen := m.TypesIndex[typeName]

	if !seen {

		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.SLICE_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.TupleValue(elems, typeId), typeId, nil
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

	return trace_record.SequenceValue(elems, true, typeId), typeId, nil

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

	return trace_record.SequenceValue(elems, true, typeId), typeId, nil

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

// Stylus contracts use https://crates.io/crates/ruint to store variables. So we should handle them.
func bytesToRuintRust(rawBytes []byte, typ *dwarf.StructType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {
	typeName := typ.String()

	rawStruct, _, err := bytesToStruct(rawBytes, typ, m)
	if err != nil {
		return nil, INVALID_TYPE_ID, err
	}

	fields := rawStruct.(trace_record.StructValueRecord).Fields

	if len(fields) != 1 {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a Uint")
	}

	limbs, ok := fields[0].(trace_record.SequenceValueRecord)
	if !ok {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not a Uint")
	}

	bytes := make([]byte, 0)
	for _, n := range limbs.Elements {
		limb, ok := n.(trace_record.IntValueRecord)
		if !ok {
			return nil, INVALID_TYPE_ID, fmt.Errorf("not a Uint")
		}

		bytes = binary.LittleEndian.AppendUint64(bytes, uint64(limb.I))
	}

	slices.Reverse(bytes)
	for len(bytes) > 1 && bytes[0] == 0 {
		bytes = bytes[1:]
	}

	typeId, seen := m.TypesIndex[typeName]

	if !seen {
		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]

		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.SLICE_TYPE_KIND, typeName)

		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.BigIntValue(bytes, false, typeId), typeId, nil
}

// bytesToAddressRust handles types that represent an EVM address. The type is
// expected to be a newtype wrapper over a 20-byte array. The bytes are returned
// as a hexadecimal string.
func bytesToAddressRust(rawBytes []byte, typ *dwarf.StructType, m *wasm.ModuleInstance) (trace_record.ValueRecord, trace_record.TypeId, error) {
	typeName := typ.String()

	rawStruct, _, err := bytesToStruct(rawBytes, typ, m)
	if err != nil {
		return nil, INVALID_TYPE_ID, err
	}

	fields := rawStruct.(trace_record.StructValueRecord).Fields
	if len(fields) != 1 {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not an address")
	}

	seq, ok := fields[0].(trace_record.SequenceValueRecord)
	if !ok {
		return nil, INVALID_TYPE_ID, fmt.Errorf("not an address")
	}

	bytes := make([]byte, 0, len(seq.Elements))
	for _, e := range seq.Elements {
		iv, ok := e.(trace_record.IntValueRecord)
		if !ok {
			return nil, INVALID_TYPE_ID, fmt.Errorf("not an address")
		}
		bytes = append(bytes, byte(iv.I))
	}

	hexStr := fmt.Sprintf("0x%x", bytes)

	typeId, seen := m.TypesIndex[typeName]
	if !seen {
		m.TypesIndex[typeName] = trace_record.TypeId(len(m.TypesIndex))
		typeId = m.TypesIndex[typeName]
		typeRecord := trace_record.NewSimpleTypeRecord(trace_record.STRING_TYPE_KIND, typeName)
		m.Record.RegisterTypeWithNewId(typeName, typeRecord)
	}

	return trace_record.StringValue(hexStr, typeId), typeId, nil
}
