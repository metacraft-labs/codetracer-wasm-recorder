package boundarylog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

// TestLoadsTheRealCtInstrumentManifest decodes the committed sidecar
// manifest — real `ct-instrument` output, not a hand-written sample — and
// asserts every field this package relies on.
func TestLoadsTheRealCtInstrumentManifest(t *testing.T) {
	m, err := LoadManifest(balanceManifest)
	require.NoError(t, err)

	require.Equal(t, 2, len(m.Paths))
	require.True(t, strings.HasSuffix(m.Paths[0], "wasm-src/lib.rs"))

	// `functions` and `sites` are index-parallel and indexed by EXPORT
	// index; non-function exports occupy their slot with an empty name.
	require.Equal(t, 4, len(m.Functions))
	require.Equal(t, 4, len(m.Sites))
	require.Equal(t, "", m.Functions[0].Name)
	require.Equal(t, "compute_balance", m.Functions[1].Name)
	require.Equal(t, int64(71), m.Functions[1].Line)

	// `boundaries` is a FLAT ARRAY looked up by (fnKind, fnIndex), not
	// index-parallel with anything.
	require.Equal(t, 1, len(m.Boundaries))
	b := m.Boundary(FuncKindExport, 1)
	require.NotNil(t, b)
	require.Equal(t, "compute_balance", b.Name)
	require.Equal(t, "", b.Module, "exports carry no module name")
	require.Equal(t, "", b.UnsupportedType)

	require.Nil(t, m.Boundary(FuncKindImport, 1),
		"the import and export index spaces are disjoint")
	require.Nil(t, m.Boundary(FuncKindExport, 0),
		"the boundaries table is sparse; export #0 is not a function")

	sig, err := b.Signature()
	require.NoError(t, err)
	require.Equal(t, Signature{
		Params:  []ScalarType{TypeI32, TypeI32},
		Results: []ScalarType{TypeI32},
	}, sig)
}

func TestExportBoundaryByName(t *testing.T) {
	m, err := LoadManifest(balanceManifest)
	require.NoError(t, err)
	require.NotNil(t, m.ExportBoundaryByName("compute_balance"))
	require.Nil(t, m.ExportBoundaryByName("nope"))
	require.Nil(t, m.ExportBoundaryByName(""))
}

// TestNilManifestLookupsAreSafe pins that the manifest is genuinely
// optional: every accessor tolerates a nil receiver, so replay does not
// need a null check at each call site.
func TestNilManifestLookupsAreSafe(t *testing.T) {
	var m *Manifest
	require.Nil(t, m.Boundary(FuncKindExport, 0))
	require.Nil(t, m.ExportBoundaryByName("x"))
}

// TestUnsupportedTypeBoundaryIsRejected pins spec §8: an edge the
// instrumenter refused to record cannot be replayed either.
func TestUnsupportedTypeBoundaryIsRejected(t *testing.T) {
	b := ManifestBoundary{
		FnKind: FuncKindImport, FnIndex: 3, Name: "take_ref", Module: "env",
		UnsupportedType: "a reference type",
	}
	_, err := b.Signature()
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "import #3 (env.take_ref)"),
		"the diagnostic must name the edge; got: %v", err)
	require.True(t, strings.Contains(err.Error(), "rather than recording it with a gap"),
		"got: %v", err)
}

func TestSignatureRejectsAnUnknownTypeSpelling(t *testing.T) {
	b := ManifestBoundary{FnKind: FuncKindExport, FnIndex: 0, Name: "f",
		Params: []string{"i32", "v128"}}
	_, err := b.Signature()
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "param 1"), "got: %v", err)
}

func TestLoadManifestReportsBadJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "m.json")
	require.NoError(t, os.WriteFile(p, []byte("{not json"), 0o644))
	_, err := LoadManifest(p)
	require.Error(t, err)

	_, err = LoadManifest(filepath.Join(t.TempDir(), "absent.json"))
	require.Error(t, err)
}

// TestFindManifestDiscoversTheConventionalSidecar covers both accepted
// layouts and, importantly, that discovery does NOT invent a path.
func TestFindManifestDiscoversTheConventionalSidecar(t *testing.T) {
	dir := t.TempDir()
	wasm := filepath.Join(dir, "m.wasm")
	require.NoError(t, os.WriteFile(wasm, []byte{0}, 0o644))

	require.Equal(t, "", FindManifest(wasm), "no sidecar yet")

	// `<module>.wasm.manifest.json` — the ct-instrument-cli default.
	primary := wasm + ".manifest.json"
	require.NoError(t, os.WriteFile(primary, []byte("{}"), 0o644))
	require.Equal(t, primary, FindManifest(wasm))
	require.Equal(t, primary, DefaultManifestPath(wasm))
	require.NoError(t, os.Remove(primary))

	// `<module>.manifest.json` — what a caller gets from an explicit
	// `--manifest foo.manifest.json` next to `foo.wasm`.
	alt := filepath.Join(dir, "m.manifest.json")
	require.NoError(t, os.WriteFile(alt, []byte("{}"), 0o644))
	require.Equal(t, alt, FindManifest(wasm))

	// A directory at the candidate path must not be mistaken for one.
	require.NoError(t, os.Remove(alt))
	require.NoError(t, os.Mkdir(alt, 0o755))
	require.Equal(t, "", FindManifest(wasm))
}
