package wasmsnapshot

import (
	"fmt"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/wasm"
)

// GlobalValue is one global's value at a quiescent point.
//
// Snapshot spec §4 captures "all mutable globals"; immutable ones are captured
// too, because they cost 20 bytes each and their presence lets `Restore` check
// that the module it is restoring into is the module the snapshot came from.
type GlobalValue struct {
	// ValueType is the api.ValueType* constant.
	ValueType api.ValueType
	// Mutable mirrors the global's declared mutability.
	Mutable bool
	// Lo is the 64-bit value; Hi is used only by v128 globals.
	Lo, Hi uint64
}

// TableValue is one table's contents at a quiescent point.
//
// Elements are stored as **function indices**, not as the engine's own
// `Reference` values. A wazero reference is a `uintptr` into engine-owned
// memory, meaningful only inside the process that minted it; writing those
// into a snapshot would produce a file that appears portable and is not.
// Snapshot spec §3 makes engine-independence the whole reason quiescent points
// were chosen, so the table encoding has to honour it too.
type TableValue struct {
	// RefType is wasm.RefTypeFuncref or wasm.RefTypeExternref.
	RefType byte
	// Elements holds one entry per table slot: a function index, or
	// NullElement for a null reference.
	Elements []int32
}

// NullElement marks an empty table slot.
const NullElement int32 = -1

// Snapshot is a module's entire state at a quiescent point.
//
// There is deliberately no execution-context field. Snapshot spec §3: at a
// quiescent point "the WASM stack is empty, so linear memory + globals +
// tables *are* the entire state". Mid-call snapshots, which would need the
// interpreter's stack and would make the format engine-specific, are out of
// scope for V2 (§11).
type Snapshot struct {
	// Point is the ordinal of the quiescent point this captures.
	Point int
	// MemoryBytes is the linear memory's size. Snapshot spec §4 records it
	// separately so a consumer can pre-size its buffer before decoding.
	MemoryBytes uint64
	// Memory is the linear memory image, always a whole number of PageSize
	// pages. Nil when the module has no memory.
	Memory []byte
	// Globals are the module's globals in index order.
	Globals []GlobalValue
	// Tables are the module's tables in index order.
	Tables []TableValue
}

// moduleInstance narrows an api.Module to the wazero-internal type the
// snapshot needs. `api.Module` exposes memories and exported globals but
// neither the module's full global vector nor its tables, and a snapshot of
// only the *exported* state would silently omit the shadow-stack pointer and
// the indirect-call table that every non-trivial module depends on.
func moduleInstance(mod api.Module) (*wasm.ModuleInstance, error) {
	mi, ok := mod.(*wasm.ModuleInstance)
	if !ok {
		return nil, fmt.Errorf(
			"wasmsnapshot: module %q is a %T, not a wazero ModuleInstance; snapshots "+
				"need access to the module's globals and tables, which api.Module does "+
				"not expose", mod.Name(), mod)
	}
	return mi, nil
}

// Capture takes a snapshot of a module at a quiescent point.
//
// The caller is responsible for only calling it at one: nothing observable
// from here distinguishes "between exported calls" from "inside a host
// function called by the module", and a snapshot taken at the latter would
// omit the live WASM stack. `boundarylog.Replay`'s `AtQuiescentPoint` hook is
// the supported call site.
func Capture(mod api.Module, point int) (*Snapshot, error) {
	mi, err := moduleInstance(mod)
	if err != nil {
		return nil, err
	}
	s := &Snapshot{Point: point}

	if mem := mi.MemoryInstance; mem != nil {
		buf := mem.Buffer
		if len(buf)%PageSize != 0 {
			return nil, fmt.Errorf(
				"wasmsnapshot: module %q has a %d-byte linear memory, not a whole "+
					"number of %d-byte WASM pages", mod.Name(), len(buf), PageSize)
		}
		s.MemoryBytes = uint64(len(buf))
		s.Memory = append([]byte(nil), buf...)
	}

	if mi.Engine != nil && mi.Engine.OwnsGlobals() {
		// The interpreter returns false here, and it is the only engine this
		// recorder runs. Refusing rather than reading the (then stale)
		// GlobalInstance.Val fields keeps a wrong snapshot from being written
		// if the recorder is ever pointed at the compiler engine.
		return nil, fmt.Errorf(
			"wasmsnapshot: module %q runs on an engine that owns its globals; their "+
				"values are held inside the engine, not in the GlobalInstance, so this "+
				"snapshot would capture stale values", mod.Name())
	}
	for _, g := range mi.Globals {
		s.Globals = append(s.Globals, GlobalValue{
			ValueType: g.Type.ValType,
			Mutable:   g.Type.Mutable,
			Lo:        g.Val,
			Hi:        g.ValHi,
		})
	}

	for i, tbl := range mi.Tables {
		tv, err := captureTable(mi, i, tbl)
		if err != nil {
			return nil, err
		}
		s.Tables = append(s.Tables, tv)
	}
	return s, nil
}

// captureTable converts a table's engine references back into function
// indices. See TableValue for why the raw references are not stored.
func captureTable(mi *wasm.ModuleInstance, idx int, tbl *wasm.TableInstance) (TableValue, error) {
	tv := TableValue{RefType: tbl.Type, Elements: make([]int32, len(tbl.References))}
	if tbl.Type == wasm.RefTypeExternref {
		// Snapshot spec §11 puts `externref` out of scope: "a reference has no
		// meaning outside the host that minted it, so neither the boundary
		// recording nor a snapshot can carry one faithfully".
		for _, ref := range tbl.References {
			if ref != 0 {
				return tv, fmt.Errorf(
					"wasmsnapshot: table %d holds a non-null externref; external "+
						"references cannot be snapshotted (snapshot spec §11)", idx)
			}
		}
		for i := range tv.Elements {
			tv.Elements[i] = NullElement
		}
		return tv, nil
	}

	// Invert the engine's function-index → Reference map. The module's own
	// function count bounds the search; a reference outside it belongs to
	// another instance and cannot be named portably.
	byRef := map[wasm.Reference]int32{}
	if mi.Source != nil && mi.Engine != nil {
		total := mi.Source.ImportFunctionCount + uint32(len(mi.Source.FunctionSection))
		for fi := uint32(0); fi < total; fi++ {
			byRef[mi.Engine.FunctionInstanceReference(fi)] = int32(fi)
		}
	}
	for i, ref := range tbl.References {
		if ref == 0 {
			tv.Elements[i] = NullElement
			continue
		}
		fi, ok := byRef[ref]
		if !ok {
			return tv, fmt.Errorf(
				"wasmsnapshot: table %d slot %d holds a function reference that does "+
					"not belong to this module; a cross-module reference has no portable "+
					"name and cannot be snapshotted", idx, i)
		}
		tv.Elements[i] = fi
	}
	return tv, nil
}

// Restore writes a snapshot back into a freshly instantiated module, making it
// resume-ready at the captured quiescent point.
//
// The module must be the same module the snapshot came from — the shape checks
// below (memory size, global count and types, table count and length) exist to
// turn "snapshot from a different build" into an immediate, named failure
// rather than a replay that diverges later for no visible reason.
func (s *Snapshot) Restore(mod api.Module) error {
	mi, err := moduleInstance(mod)
	if err != nil {
		return err
	}

	if s.Memory != nil {
		mem := mi.MemoryInstance
		if mem == nil {
			return fmt.Errorf(
				"wasmsnapshot: the snapshot carries %d bytes of linear memory but "+
					"module %q has none", len(s.Memory), mod.Name())
		}
		if uint64(len(mem.Buffer)) > s.MemoryBytes {
			return fmt.Errorf(
				"wasmsnapshot: module %q was instantiated with %d bytes of memory but "+
					"the snapshot records %d; restoring would leave the tail of the "+
					"memory holding content from no defined point in the execution",
				mod.Name(), len(mem.Buffer), s.MemoryBytes)
		}
		// A snapshot taken after `memory.grow` is larger than a fresh
		// instantiation. Grow the instance to match before writing.
		if uint64(len(mem.Buffer)) < s.MemoryBytes {
			need := uint32((s.MemoryBytes - uint64(len(mem.Buffer))) / PageSize)
			if _, ok := mem.Grow(need); !ok {
				return fmt.Errorf(
					"wasmsnapshot: module %q's memory cannot grow to the snapshot's %d "+
						"bytes (currently %d, max %d)",
					mod.Name(), s.MemoryBytes, len(mem.Buffer), mem.Max)
			}
		}
		copy(mem.Buffer, s.Memory)
	} else if mi.MemoryInstance != nil && len(mi.MemoryInstance.Buffer) != 0 {
		return fmt.Errorf(
			"wasmsnapshot: module %q has a linear memory but the snapshot carries none",
			mod.Name())
	}

	if mi.Engine != nil && mi.Engine.OwnsGlobals() {
		return fmt.Errorf(
			"wasmsnapshot: module %q runs on an engine that owns its globals; writing "+
				"GlobalInstance.Val would not reach the engine's copy", mod.Name())
	}
	if len(s.Globals) != len(mi.Globals) {
		return fmt.Errorf(
			"wasmsnapshot: the snapshot has %d global(s) but module %q has %d; the "+
				"snapshot was taken from a different build",
			len(s.Globals), mod.Name(), len(mi.Globals))
	}
	for i, g := range s.Globals {
		inst := mi.Globals[i]
		if inst.Type.ValType != g.ValueType {
			return fmt.Errorf(
				"wasmsnapshot: global %d is type %#x in module %q but %#x in the "+
					"snapshot; the snapshot was taken from a different build",
				i, inst.Type.ValType, mod.Name(), g.ValueType)
		}
		inst.Val, inst.ValHi = g.Lo, g.Hi
	}

	if len(s.Tables) != len(mi.Tables) {
		return fmt.Errorf(
			"wasmsnapshot: the snapshot has %d table(s) but module %q has %d",
			len(s.Tables), mod.Name(), len(mi.Tables))
	}
	for i, tv := range s.Tables {
		tbl := mi.Tables[i]
		if len(tv.Elements) != len(tbl.References) {
			return fmt.Errorf(
				"wasmsnapshot: table %d has %d slot(s) in the snapshot but %d in "+
					"module %q", i, len(tv.Elements), len(tbl.References), mod.Name())
		}
		for j, fi := range tv.Elements {
			if fi == NullElement {
				tbl.References[j] = 0
				continue
			}
			tbl.References[j] = mi.Engine.FunctionInstanceReference(uint32(fi))
		}
	}
	return nil
}
