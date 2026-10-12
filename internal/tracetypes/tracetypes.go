// Package tracetypes provides the value/type/event vocabulary the wazero
// CodeTracer fork uses to describe trace events.  These types travel from
// the WASM interpreter and Stylus host stubs through the [TraceRecorder]
// interface in package tracewriter to the cgo bridge that calls into
// codetracer-trace-format-nim's CTFS writer.
//
// History: this vocabulary used to live in github.com/metacraft-labs/
// trace_record (a separate Go module shared with other recorders).  Since
// the wazero fork is the only consumer of these types and writes traces
// directly through the Nim FFI (not through trace_record's JSON writer),
// the types are inlined here so the wasm-recorder can drop the cross-repo
// Go-module dependency.  See AUDIT-CTFS-2026-05.md for the broader CTFS-
// only convention compliance pass.
package tracetypes

import (
	"encoding/json"
	"strconv"
)

// ----- ID newtypes -----

type FunctionId uint64
type CallId uint64
type VariableId uint64
type StepId uint64
type PathId uint64
type Line int64
type TypeId uint64

// ----- Type kinds -----

type TypeKind uint8

const (
	INT_TYPE_KIND     = TypeKind(7)
	FLOAT_TYPE_KIND   = TypeKind(8)
	POINTER_TYPE_KIND = TypeKind(23)
	TUPLE_TYPE_KIND   = TypeKind(27)
	ARRAY_TYPE_KIND   = TypeKind(4)
	SLICE_TYPE_KIND   = TypeKind(33)
	BOOL_TYPE_KIND    = TypeKind(12)
	STRING_TYPE_KIND  = TypeKind(9)
	STRUCT_TYPE_KIND  = TypeKind(6)
)

type TypeSpecificInfo interface {
	IsTypeSpecificInfo()
}

type NoneTypeSpecificInfo struct {
	Kind string `json:"kind"`
}

func (i NoneTypeSpecificInfo) IsTypeSpecificInfo() {}

func NewNonTypeSpecificInfo() NoneTypeSpecificInfo {
	return NoneTypeSpecificInfo{"None"}
}

type TypeRecord struct {
	Kind         TypeKind         `json:"kind"`
	LangType     string           `json:"lang_type"`
	SpecificInfo TypeSpecificInfo `json:"specific_info"`
}

func NewSimpleTypeRecord(kind TypeKind, langType string) TypeRecord {
	return TypeRecord{kind, langType, NewNonTypeSpecificInfo()}
}

func NewTypeRecord(kind TypeKind, langType string, specificInfo TypeSpecificInfo) TypeRecord {
	return TypeRecord{kind, langType, specificInfo}
}

type FieldTypeRecord struct {
	Name   string `json:"name"`
	TypeId TypeId `json:"type_id"`
}

func NewFieldTypeRecord(name string, typeId TypeId) FieldTypeRecord {
	return FieldTypeRecord{name, typeId}
}

type StructTypeInfo struct {
	Kind   string            `json:"kind"`
	Fields []FieldTypeRecord `json:"fields"`
}

func (i StructTypeInfo) IsTypeSpecificInfo() {}

func NewStructTypeInfo(fields []FieldTypeRecord) StructTypeInfo {
	return StructTypeInfo{Kind: "Struct", Fields: fields}
}

type PointerTypeInfo struct {
	Kind              string `json:"kind"`
	DereferenceTypeId TypeId `json:"dereference_type_id"`
}

func (i PointerTypeInfo) IsTypeSpecificInfo() {}

func NewPointerTypeInfo(typeId TypeId) PointerTypeInfo {
	return PointerTypeInfo{"Pointer", typeId}
}

// ----- Value records -----

type ValueRecord interface {
	IsValueRecord()
}

type NilValueRecord struct {
	Kind   string `json:"kind"`
	TypeId TypeId `json:"type_id"`
}

func (n NilValueRecord) IsValueRecord() {}

func NilValue() NilValueRecord {
	return NilValueRecord{"None", TypeId(0)}
}

type IntValueRecord struct {
	Kind   string `json:"kind"`
	I      int64  `json:"i"`
	TypeId TypeId `json:"type_id"`
}

func (i IntValueRecord) IsValueRecord() {}

func IntValue(i int64, typeId TypeId) IntValueRecord {
	return IntValueRecord{"Int", i, typeId}
}

type FloatValueRecord struct {
	Kind   string  `json:"kind"`
	F      float64 `json:"-"`
	TypeId TypeId  `json:"type_id"`
}

func (i FloatValueRecord) IsValueRecord() {}

// MarshalJSON serializes the float value with the "f" field as a string,
// matching the Rust canonical format (serde_with::DisplayFromStr).
func (r FloatValueRecord) MarshalJSON() ([]byte, error) {
	type Alias struct {
		Kind   string `json:"kind"`
		F      string `json:"f"`
		TypeId TypeId `json:"type_id"`
	}
	return json.Marshal(Alias{
		Kind:   r.Kind,
		F:      strconv.FormatFloat(r.F, 'f', -1, 64),
		TypeId: r.TypeId,
	})
}

// UnmarshalJSON deserializes the float value, accepting "f" as either
// a JSON string or a JSON number for backward compatibility.
func (r *FloatValueRecord) UnmarshalJSON(data []byte) error {
	type Alias struct {
		Kind   string          `json:"kind"`
		F      json.RawMessage `json:"f"`
		TypeId TypeId          `json:"type_id"`
	}
	var a Alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	r.Kind = a.Kind
	r.TypeId = a.TypeId
	var s string
	if err := json.Unmarshal(a.F, &s); err == nil {
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return err
		}
		r.F = f
	} else {
		var f float64
		if err := json.Unmarshal(a.F, &f); err != nil {
			return err
		}
		r.F = f
	}
	return nil
}

func FloatValue(f float64, typeId TypeId) FloatValueRecord {
	return FloatValueRecord{"Float", f, typeId}
}

type BoolValueRecord struct {
	Kind   string `json:"kind"`
	B      bool   `json:"b"`
	TypeId TypeId `json:"type_id"`
}

func (b BoolValueRecord) IsValueRecord() {}

func BoolValue(b bool, typeId TypeId) BoolValueRecord {
	return BoolValueRecord{"Bool", b, typeId}
}

type StringValueRecord struct {
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	TypeId TypeId `json:"type_id"`
}

func (s StringValueRecord) IsValueRecord() {}

func StringValue(text string, typeId TypeId) StringValueRecord {
	return StringValueRecord{"String", text, typeId}
}

type StructValueRecord struct {
	Kind   string        `json:"kind"`
	Fields []ValueRecord `json:"field_values"`
	TypeId TypeId        `json:"type_id"`
}

func (s StructValueRecord) IsValueRecord() {}

func StructValue(fields []ValueRecord, typeId TypeId) StructValueRecord {
	return StructValueRecord{"Struct", fields, typeId}
}

type SequenceValueRecord struct {
	Kind     string        `json:"kind"`
	Elements []ValueRecord `json:"elements"`
	IsSlice  bool          `json:"is_slice"`
	TypeId   TypeId        `json:"type_id"`
}

func (s SequenceValueRecord) IsValueRecord() {}

func SequenceValue(elements []ValueRecord, isSlice bool, typeId TypeId) SequenceValueRecord {
	return SequenceValueRecord{"Sequence", elements, isSlice, typeId}
}

type ReferenceValueRecord struct {
	Kind         string      `json:"kind"`
	Dereferenced ValueRecord `json:"dereferenced"`
	Address      uint64      `json:"address"`
	Mutable      bool        `json:"mutable"`
	TypeId       TypeId      `json:"type_id"`
}

func (s ReferenceValueRecord) IsValueRecord() {}

func ReferenceValue(dereferenced ValueRecord, address uint64, mutable bool, typeId TypeId) ReferenceValueRecord {
	return ReferenceValueRecord{"Reference", dereferenced, address, mutable, typeId}
}

type TupleValueRecord struct {
	Kind     string        `json:"kind"`
	Elements []ValueRecord `json:"elements"`
	TypeId   TypeId        `json:"type_id"`
}

func (s TupleValueRecord) IsValueRecord() {}

func TupleValue(elements []ValueRecord, typeId TypeId) TupleValueRecord {
	return TupleValueRecord{"Tuple", elements, typeId}
}

type BigIntValueRecord struct {
	Kind     string `json:"kind"`
	Bytes    []byte `json:"b"`
	Negative bool   `json:"negative"`
	TypeId   TypeId `json:"type_id"`
}

func (s BigIntValueRecord) IsValueRecord() {}

func BigIntValue(bytes []byte, negative bool, typeId TypeId) BigIntValueRecord {
	return BigIntValueRecord{"BigInt", bytes, negative, typeId}
}

// FullValueRecord pairs a variable id with its value — emitted on each
// step to record local-variable state and on each call to record argument
// bindings.
type FullValueRecord struct {
	VariableId VariableId  `json:"variable_id"`
	Value      ValueRecord `json:"value"`
}

// ----- Special-event kinds (I/O, errors, EVM) -----

// RecordEventKind is the `EventLogKind` of a special event: the u8 `kind` of
// an `events.dat` record (codetracer-trace-format-spec trace-events.md
// §"EventLogKind (u8 enum)").  The ordinals are the trace format's, so a
// kind is passed to the writer as is.  A recorder uses the exact kind;
// WriteOther / ReadOther are only for an operation no other kind describes.
type RecordEventKind int

const (
	// EventKindWrite is output to stdout, or a write with no known target.
	EventKindWrite RecordEventKind = iota
	EventKindWriteFile
	EventKindWriteOther
	// EventKindRead is input from stdin, or a read with no known source.
	EventKindRead
	EventKindReadFile
	EventKindReadOther
	EventKindReadDir
	EventKindOpenDir
	EventKindCloseDir
	EventKindSocket
	EventKindOpen
	// errors / exceptions / signals
	EventKindError
	// trace events
	EventKindTraceLogEvent
	EventKindEvmEvent
	// EventKindWriteStderr is output to stderr.
	EventKindWriteStderr
	// EventKindExec is running a program or spawning a process.
	EventKindExec
	// EventKindReadEnv is reading an environment variable or testing
	// whether one is set.
	EventKindReadEnv
	// EventKindWriteEnv is setting or removing an environment variable.
	EventKindWriteEnv
	// EventKindStat is an existence or metadata query of a filesystem
	// entry, including a PATH search.
	EventKindStat
	// EventKindCreateDir is creating a directory, including copying a
	// directory tree.
	EventKindCreateDir
	EventKindRemoveDir
	EventKindRemoveFile
	// EventKindRename is renaming or moving a file or directory.
	EventKindRename
	// EventKindChangeDir is changing the working directory.
	EventKindChangeDir
)

// recordEventKindNames are the trace format's names of the assigned kinds,
// indexed by ordinal.
var recordEventKindNames = [...]string{
	"Write", "WriteFile", "WriteOther", "Read", "ReadFile", "ReadOther",
	"ReadDir", "OpenDir", "CloseDir", "Socket", "Open", "Error",
	"TraceLogEvent", "EvmEvent", "WriteStderr", "Exec", "ReadEnv",
	"WriteEnv", "Stat", "CreateDir", "RemoveDir", "RemoveFile", "Rename",
	"ChangeDir",
}

// AllRecordEventKinds lists every assigned kind in ordinal order.  Values
// outside it are unassigned: the writer refuses them rather than storing a
// substitute kind.
var AllRecordEventKinds = func() []RecordEventKind {
	all := make([]RecordEventKind, len(recordEventKindNames))
	for i := range all {
		all[i] = RecordEventKind(i)
	}
	return all
}()

// Valid reports whether k is a kind the trace format assigns.
func (k RecordEventKind) Valid() bool {
	return k >= 0 && int(k) < len(recordEventKindNames)
}

// String returns the trace format's name of k, the name readers such as
// `ct-print` report it under.  An unassigned value is spelled so that it
// cannot be mistaken for a kind.
func (k RecordEventKind) String() string {
	if !k.Valid() {
		return "RecordEventKind(unassigned " + strconv.Itoa(int(k)) + ")"
	}
	return recordEventKindNames[k]
}
