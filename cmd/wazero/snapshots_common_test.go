package main

import (
	"bytes"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

// The flag-validation surface is identical in both build variants, so this
// file carries no build tag: it compiles and runs against the open artifact
// and the snapshot-deriving one alike.

// TestSnapshotFlagValidationIsBuildIndependent: the flag combinations that
// make no sense are rejected identically in both variants, so a user's
// muscle memory transfers.
func TestSnapshotFlagValidationIsBuildIndependent(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts snapshotOptions
		want string
	}{
		{"seek without source", snapshotOptions{from: 2, every: 1}, "--snapshot-source"},
		{"empty range", snapshotOptions{from: 2, to: 2, source: "x.ct", every: 1}, "--seek-to"},
		{"negative", snapshotOptions{from: -2, every: 1}, "cannot be negative"},
		{"source without seek", snapshotOptions{from: seekUnset, source: "x.ct", every: 1}, "no effect"},
		{"bad density", snapshotOptions{from: seekUnset, every: 0}, "--snapshot-every"},
		{"promote without derive", snapshotOptions{from: seekUnset, promote: true, every: 1}, "--cas-share-system"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.validate()
			require.Error(t, err)
			require.True(t, bytes.Contains([]byte(err.Error()), []byte(tc.want)),
				"error %q does not mention %q", err, tc.want)
		})
	}
}
