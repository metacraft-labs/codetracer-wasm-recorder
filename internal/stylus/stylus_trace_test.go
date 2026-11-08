package stylus

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/internal/testing/require"
)

// TestInstantiateReportsWhyAnEventIsNotAnObject loads a Stylus trace whose
// event is a JSON array instead of an object. `evmEvent.UnmarshalJSON` decodes
// each event into a map first; when that decode's error was dropped, the map
// stayed nil and the operator was told "name is not string" about an event
// that has no fields at all. The error has to be the decoder's own, which
// names what was found and where.
//
// No mocks: the trace goes through `Instantiate`, the entry point `wazero run
// --stylus` uses, against a real runtime.
func TestInstantiateReportsWhyAnEventIsNotAnObject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.json")
	require.NoError(t, os.WriteFile(path, []byte(`[["user_entrypoint", 0, 0, "0x", "0x"]]`), 0o600))

	ctx := context.Background()
	r := wazero.NewRuntime(ctx)
	defer func() { _ = r.Close(ctx) }()

	_, err := Instantiate(ctx, r, path, nil)

	var typeErr *json.UnmarshalTypeError
	require.True(t, errors.As(err, &typeErr),
		"expected the JSON decoder's type error for a non-object event, got: %v", err)
	require.Equal(t, "array", typeErr.Value)
}
