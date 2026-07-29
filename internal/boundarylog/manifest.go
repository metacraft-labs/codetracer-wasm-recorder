package boundarylog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Function-kind discriminants, mirroring `hooks.rs`:
//
//	pub const FUNC_KIND_IMPORT: i32 = 0;
//	pub const FUNC_KIND_EXPORT: i32 = 1;
//	pub const FUNC_KIND_STORE:  i32 = 2;   // withdrawn experimental pass
const (
	FuncKindImport = 0
	FuncKindExport = 1
	FuncKindStore  = 2
)

// Manifest is the `ct-instrument` sidecar manifest, emitted next to the
// instrumented module as `<output>.wasm.manifest.json`.
//
// Field names follow `ModuleManifest` in
// `codetracer-wasm-instrumenter/crates/codetracer-wasm-instrumenter/src/manifest.rs`:
// the root object's own fields are single lowercase words, while the nested
// records carry `#[serde(rename_all = "camelCase")]`.
type Manifest struct {
	Paths      []string           `json:"paths"`
	Functions  []ManifestFunction `json:"functions"`
	Sites      []ManifestSite     `json:"sites"`
	Boundaries []ManifestBoundary `json:"boundaries"`
}

// ManifestFunction names an export slot. Index-parallel with Sites and
// indexed by *export index*; non-function exports still occupy their slot,
// emitted with an empty name rather than skipped.
type ManifestFunction struct {
	Name      string `json:"name"`
	PathIndex int    `json:"pathIndex"`
	Line      int64  `json:"line"`
	Col       int64  `json:"col"`
}

// ManifestSite is a source position the instrumenter attributes a step to.
type ManifestSite struct {
	Kind      string `json:"kind"`
	PathIndex int    `json:"pathIndex"`
	Line      int64  `json:"line"`
	Col       int64  `json:"col"`
	FnID      int    `json:"fnId"`
}

// ManifestBoundary carries one boundary edge's signature.
//
// The `boundaries` table is a flat JSON array, *not* index-parallel with
// anything: an entry is looked up by its `(fnKind, fnIndex)` pair, because
// the import and export index spaces are disjoint and both are sparse in
// it. `Manifest.Boundary` does that lookup.
type ManifestBoundary struct {
	// FnKind is FuncKindImport or FuncKindExport.
	FnKind int `json:"fnKind"`
	// FnIndex is the import index (counting only function imports, and
	// skipping the `__codetracer` hooks) or the export index (counting
	// every export, function or not).
	FnIndex uint32 `json:"fnIndex"`
	// Name is the import's field name, or the export's name.
	Name string `json:"name"`
	// Module is the import's module name; empty for exports.
	Module string `json:"module"`
	// Params and Results are lowercase type spellings ("i32", …).
	Params  []string `json:"params"`
	Results []string `json:"results"`
	// UnsupportedType is set, and Params/Results are left empty, when the
	// edge's signature mentions a type no value hook can carry. Such a
	// boundary was rejected at instrumentation time (spec §8); seeing one
	// here means the recording cannot be replayed.
	UnsupportedType string `json:"unsupportedType,omitempty"`
}

// Signature converts the boundary's recorded type spellings into a
// Signature, rejecting an edge the instrumenter marked unsupported.
func (b ManifestBoundary) Signature() (Signature, error) {
	if b.UnsupportedType != "" {
		return Signature{}, fmt.Errorf(
			"boundary %s declares unsupported type %q: the instrumenter "+
				"rejected this edge rather than recording it with a gap (spec §8)",
			b.describe(), b.UnsupportedType)
	}
	var sig Signature
	for i, s := range b.Params {
		t, err := ParseScalarType(s)
		if err != nil {
			return Signature{}, fmt.Errorf("boundary %s param %d: %w", b.describe(), i, err)
		}
		sig.Params = append(sig.Params, t)
	}
	for i, s := range b.Results {
		t, err := ParseScalarType(s)
		if err != nil {
			return Signature{}, fmt.Errorf("boundary %s result %d: %w", b.describe(), i, err)
		}
		sig.Results = append(sig.Results, t)
	}
	return sig, nil
}

func (b ManifestBoundary) describe() string {
	switch b.FnKind {
	case FuncKindImport:
		if b.Module != "" {
			return fmt.Sprintf("import #%d (%s.%s)", b.FnIndex, b.Module, b.Name)
		}
		return fmt.Sprintf("import #%d (%s)", b.FnIndex, b.Name)
	case FuncKindExport:
		return fmt.Sprintf("export #%d (%s)", b.FnIndex, b.Name)
	default:
		return fmt.Sprintf("fnKind=%d #%d (%s)", b.FnKind, b.FnIndex, b.Name)
	}
}

// Boundary looks up the entry for one `(fnKind, fnIndex)` pair, returning
// nil when the manifest carries none.
func (m *Manifest) Boundary(fnKind int, fnIndex uint32) *ManifestBoundary {
	if m == nil {
		return nil
	}
	for i := range m.Boundaries {
		if m.Boundaries[i].FnKind == fnKind && m.Boundaries[i].FnIndex == fnIndex {
			return &m.Boundaries[i]
		}
	}
	return nil
}

// ExportBoundaryByName looks up an export edge by its exported name, which
// is how a boundary recording identifies one: `browser_session.js` labels
// an export's value bindings with `manifest.functions[fnIndex].name`, not
// with the index.
func (m *Manifest) ExportBoundaryByName(name string) *ManifestBoundary {
	if m == nil || name == "" {
		return nil
	}
	for i := range m.Boundaries {
		if m.Boundaries[i].FnKind == FuncKindExport && m.Boundaries[i].Name == name {
			return &m.Boundaries[i]
		}
	}
	return nil
}

// LoadManifest reads and decodes a `ct-instrument` sidecar manifest.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading instrumentation manifest: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decoding instrumentation manifest %s: %w", path, err)
	}
	return &m, nil
}

// DefaultManifestPath is the conventional sidecar location for a module:
// `<module>.wasm.manifest.json`, i.e. the module path with
// `.manifest.json` appended. That is what
// `ct-instrument-cli`'s `--manifest` default produces.
func DefaultManifestPath(wasmPath string) string {
	return wasmPath + ".manifest.json"
}

// FindManifest returns the sidecar manifest path for a module if one exists
// at the conventional location, or "" when there is none. Discovery is
// deliberately narrow — a wrong manifest is worse than no manifest, because
// its signatures would be cross-checked against the module and produce a
// confusing hard error.
func FindManifest(wasmPath string) string {
	candidate := DefaultManifestPath(wasmPath)
	if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
		return candidate
	}
	// Also accept `<basename-without-extension>.manifest.json`, which is
	// what a caller gets from `--manifest foo.manifest.json` next to
	// `foo.wasm`.
	ext := filepath.Ext(wasmPath)
	if ext != "" {
		candidate = wasmPath[:len(wasmPath)-len(ext)] + ".manifest.json"
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate
		}
	}
	return ""
}
