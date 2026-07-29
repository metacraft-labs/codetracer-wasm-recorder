// Unit tests for the slice bookkeeping that `internal/boundarylog`'s
// end-to-end slice tests cannot reach.
//
// NO MOCKS. `checkTiling` is a pure function over the manifest records
// `SliceWriter` builds, so these tests hand it those records directly. That is
// the only way to exercise it at all: `SliceWriter.AtQuiescentPoint` opens each
// slice at the exact point the previous one was sealed at and refuses a point
// that does not advance, so a gap or an overlap cannot be produced through the
// public API. A defensive check no test can reach is indistinguishable from one
// that does not work, which is why these exist.
package wasmsnapshot

import (
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

func TestTilingAcceptsAPartition(t *testing.T) {
	require.NoError(t, checkTiling(nil))
	require.NoError(t, checkTiling([]SliceInfo{
		{Index: 0, BasePoint: 0, EndPoint: 2},
	}))
	require.NoError(t, checkTiling([]SliceInfo{
		{Index: 0, BasePoint: 0, EndPoint: 2},
		{Index: 1, BasePoint: 2, EndPoint: 4},
		{Index: 2, BasePoint: 4, EndPoint: 5},
	}))
}

// TestTilingRejectsAGap: slice 1 starting past slice 0's end would lose the
// exported calls in between, and no consumer of the manifest could tell.
func TestTilingRejectsAGap(t *testing.T) {
	err := checkTiling([]SliceInfo{
		{Index: 0, BasePoint: 0, EndPoint: 2},
		{Index: 1, BasePoint: 3, EndPoint: 5},
	})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "does not tile"), err.Error())
	require.True(t, strings.Contains(err.Error(), "starts at quiescent point 3"), err.Error())
}

// TestTilingRejectsAnOverlap: slice 1 starting before slice 0's end would
// materialise the same calls twice.
func TestTilingRejectsAnOverlap(t *testing.T) {
	err := checkTiling([]SliceInfo{
		{Index: 0, BasePoint: 0, EndPoint: 3},
		{Index: 1, BasePoint: 2, EndPoint: 5},
	})
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "does not tile"), err.Error())
	require.True(t, strings.Contains(err.Error(), "ended at 3"), err.Error())
}

// TestSliceContainerNaming pins the relationship between the program name a
// slice's trace is produced under and the container filename that results, on
// which the manifest's `File` field and `TestSliceIsAValidStandaloneContainer`
// both depend.
func TestSliceContainerNaming(t *testing.T) {
	require.Equal(t, "balance_calc-0000.wasm", SliceProgramName("balance_calc.wasm", 0))
	require.Equal(t, "balance_calc-0012.wasm", SliceProgramName("balance_calc.wasm", 12))
	require.Equal(t, "balance_calc-0000.ct", sliceContainerName("balance_calc.wasm", 0))
	// A path is reduced to its base, so a slice never inherits a directory.
	require.Equal(t, "m-0003.ct", sliceContainerName("/a/b/m.wasm", 3))
	// A program name with no extension still yields a distinct container.
	require.Equal(t, "recording-0001.ct", sliceContainerName("recording", 1))
}

// TestSlicePolicySealsAfter pins the two triggers independently, including that
// the zero policy never seals early — the "possibly absent entirely" slice unit
// of `WASM-Replay-Snapshots-And-Slices.md` §7.
func TestSlicePolicySealsAfter(t *testing.T) {
	var zero SlicePolicy
	require.False(t, zero.sealsAfter(0, 0))
	require.False(t, zero.sealsAfter(1_000_000, 1<<40))

	count := SlicePolicy{EveryPoints: 3}
	require.False(t, count.sealsAfter(2, 1<<40))
	require.True(t, count.sealsAfter(3, 0))

	size := SlicePolicy{TargetBytes: 1024}
	require.False(t, size.sealsAfter(1_000_000, 1023))
	require.True(t, size.sealsAfter(0, 1024))

	// Either trigger alone is enough when both are set.
	both := SlicePolicy{EveryPoints: 10, TargetBytes: 1024}
	require.True(t, both.sealsAfter(10, 0))
	require.True(t, both.sealsAfter(0, 2048))
	require.False(t, both.sealsAfter(9, 1023))
}
