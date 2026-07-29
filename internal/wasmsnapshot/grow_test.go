package wasmsnapshot_test

// This file is in the external test package because it needs a live wazero
// runtime, and `wazero` imports the internals `wasmsnapshot` also imports.
//
// NO MOCKS: a real interpreter instantiates the real committed
// `testdata/grow_mem.wasm`, really calls `memory.grow`, and the snapshot is
// really round-tripped through the CAS streams and a real `.ct` container.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasmsnapshot"
)

// instantiate returns a fresh instance of the growing-memory module.
func instantiate(t *testing.T, ctx context.Context, rt wazero.Runtime, name string) api.Module {
	t.Helper()
	bin, err := os.ReadFile(filepath.Join("testdata", "grow_mem.wasm"))
	require.NoError(t, err)
	mod, err := rt.InstantiateWithConfig(ctx, bin,
		wazero.NewModuleConfig().WithName(name).WithStartFunctions())
	require.NoError(t, err)
	return mod
}

func callOne(t *testing.T, ctx context.Context, mod api.Module, fn string, args ...uint64) uint64 {
	t.Helper()
	res, err := mod.ExportedFunction(fn).Call(ctx, args...)
	require.NoError(t, err)
	require.Equal(t, 1, len(res))
	return res[0]
}

// TestSnapshotOfAGrownMemoryRestoresIntoAFreshInstance is the `memory.grow`
// case snapshot spec §4 and §5 both rest on: the snapshot records the memory
// *size*, and restoring it into a freshly instantiated module — which starts at
// the module's declared minimum — must grow that module to match before writing
// the image. Nothing else in this repository exercises it: the committed
// `balance_calc.wasm` never grows its memory.
func TestSnapshotOfAGrownMemoryRestoresIntoAFreshInstance(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer func() { _ = rt.Close(ctx) }()

	live := instantiate(t, ctx, rt, "live")
	// Three grows: the memory ends up four pages long with a distinct marker
	// at the head of each page it added.
	for i, v := range []uint64{0xaaaa, 0xbbbb, 0xcccc} {
		require.Equal(t, uint64(i+1), callOne(t, ctx, live, "bump", v))
	}
	require.Equal(t, uint64(4), callOne(t, ctx, live, "size"))

	snap, err := wasmsnapshot.Capture(live, 3)
	require.NoError(t, err)
	require.Equal(t, uint64(4*wasmsnapshot.PageSize), snap.MemoryBytes)

	// Round-trip through the real streams and a real container, so what is
	// restored is what a seek would actually read back rather than the
	// in-memory object.
	points := []wasmsnapshot.QuiescentPoint{{Ordinal: 3, CrossingSeq: -1}}
	b, err := wasmsnapshot.NewBuilder(points, wasmsnapshot.NewTiers(false),
		wasmsnapshot.EncodeOptions{})
	require.NoError(t, err)
	require.NoError(t, b.Add(snap))
	path := filepath.Join(t.TempDir(), "grown.ct")
	c, err := ctfs.Create(path, 4096)
	require.NoError(t, err)
	require.NoError(t, c.AddFiles(map[string][]byte{"meta.dat": []byte("x")}))
	require.NoError(t, b.AttachTo(path))

	set, diag, err := wasmsnapshot.Load(path, false)
	require.NoError(t, err)
	require.Equal(t, "", diag)
	rec, ok := set.Nearest(3)
	require.True(t, ok)
	readBack, err := set.Snapshot(rec)
	require.NoError(t, err)
	require.Equal(t, snap.MemoryBytes, readBack.MemoryBytes)

	// A fresh instance starts at the declared minimum of one page.
	fresh := instantiate(t, ctx, rt, "fresh")
	require.Equal(t, uint64(1), callOne(t, ctx, fresh, "size"))
	require.NoError(t, readBack.Restore(fresh))

	require.Equal(t, uint64(4), callOne(t, ctx, fresh, "size"))
	require.Equal(t, uint64(3), callOne(t, ctx, fresh, "calls"))
	after, err := wasmsnapshot.Capture(fresh, 3)
	require.NoError(t, err)
	require.Equal(t, snap.MemoryBytes, after.MemoryBytes)
	if !bytes.Equal(snap.Memory, after.Memory) {
		t.Fatal("the restored memory differs from the captured one")
	}
	require.Equal(t, snap.Globals, after.Globals)

	// And it resumes correctly: the next grow continues the sequence rather
	// than starting over.
	require.Equal(t, uint64(4), callOne(t, ctx, fresh, "bump", 0xdddd))
}

// TestRestoreRefusesToShrinkAMemory: CTFS snapshots have no way to un-grow a
// memory, so restoring a small snapshot into an already-larger instance would
// silently leave the tail holding bytes from no point in the execution.
func TestRestoreRefusesToShrinkAMemory(t *testing.T) {
	ctx := context.Background()
	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfigInterpreter())
	defer func() { _ = rt.Close(ctx) }()

	small := instantiate(t, ctx, rt, "small")
	snap, err := wasmsnapshot.Capture(small, 0)
	require.NoError(t, err)

	big := instantiate(t, ctx, rt, "big")
	callOne(t, ctx, big, "bump", 1)
	err = snap.Restore(big)
	require.Error(t, err)
	require.True(t, bytes.Contains([]byte(err.Error()), []byte("holding content from no defined point")),
		"unhelpful refusal: %v", err)
}
