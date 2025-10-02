package main

import (
	"os"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/maintester"
	"github.com/tetratelabs/wazero/internal/testing/require"
)

// Test_main ensures the following will work:
//
// go run assemblyscript.go 7
func Test_main(t *testing.T) {
	stdout, stderr := maintester.TestMain(t, main, "assemblyscript", "7")
	require.Equal(t, "hello_world returned: 10", stdout)

	// Keep stderr expectations in a fixture so DWARF/tracing warnings remain accounted for.
	expectedStderr, err := os.ReadFile("testdata/expected-stderr.txt")
	require.NoError(t, err)
	require.Equal(t, string(expectedStderr), stderr)
}
