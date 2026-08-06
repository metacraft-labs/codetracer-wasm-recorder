//go:build !cgo

// CGO-disabled fallback for the CTFS container writer.
//
// There is no pure-Go CTFS writer to fall back to, and that is the point of
// M38b: the recorder used to carry one and it drifted from the canonical
// implementation in a way that silently mis-wrote every internal file past
// ~2 MB. A non-cgo build therefore cannot attach snapshot streams to a
// container, and says so loudly rather than writing something plausible.
//
// Production builds always enable cgo via the Nix flake (see wazero.nix);
// this file exists so `go build`/`go vet` against pure-Go targets still
// resolve the package.
package ctfsffi

import "fmt"

const noWriter = "ctfsffi: this binary was built without cgo, so it has no CTFS " +
	"container writer (the writer lives in codetracer-trace-format-nim and is " +
	"reached through the C FFI); rebuild with CGO_ENABLED=1"

// Create is unavailable without cgo.
func Create(path string, blockSize uint32) error {
	return fmt.Errorf("%s: cannot create %s", noWriter, path)
}

// Append is unavailable without cgo.
func Append(path string, files map[string][]byte) error {
	if len(files) == 0 {
		return nil
	}
	return fmt.Errorf("%s: cannot append %d internal file(s) to %s",
		noWriter, len(files), path)
}
