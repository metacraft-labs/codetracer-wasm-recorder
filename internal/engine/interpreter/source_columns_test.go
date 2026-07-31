package interpreter

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

// readSourceLineLengths and addressableColumn together decide which
// DWARF columns reach the trace.  They are on the recording hot path and
// a mistake in either shows up as a *line* number the reader cannot
// place — the column axis is a byte offset into the source, recovered by
// prefix-summing the per-line table, so an entry that miscounts shifts
// every position after it and a column outside the table lands on the
// wrong line or off the end of the file entirely.
//
// The end-to-end coverage (cmd/wazero/recorder_defects_test.go) drives
// only LF-terminated fixtures, so the edges below — CRLF, an empty line,
// a one-byte line, the last line, and a path with no table at all — are
// pinned here directly.

// TestReadSourceLineLengthsCountsBytes pins the table as a *byte* map of
// the file.  A CRLF terminator's `\r` is counted, matching the
// cross-recorder convention: codetracer-cairo-recorder's
// `source_map.rs::test_line_lengths_crlf_counts_cr` asserts
// `"abc\r\ndef\n"` -> `[4, 3]`, and the Solana recorder's
// `read_line_lengths_for_path` counts every byte that is not `\n`.
func TestReadSourceLineLengthsCountsBytes(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []uint32
	}{
		{"lf", "abc\ndef\n", []uint32{3, 3, 0}},
		{"crlf counts the cr", "abc\r\ndef\n", []uint32{4, 3, 0}},
		{"no trailing newline", "abc\ndef", []uint32{3, 3}},
		{"empty line in the middle", "abc\n\ndef\n", []uint32{3, 0, 3, 0}},
		{"empty file", "", []uint32{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "src.rs")
			require.NoError(t, os.WriteFile(path, []byte(tc.body), 0o600))
			require.Equal(t, tc.want, readSourceLineLengths(path))
		})
	}

	require.Nil(t, readSourceLineLengths(filepath.Join(t.TempDir(), "absent.rs")),
		"an unreadable path must yield no table rather than an empty one that "+
			"claims every line is zero bytes long")
}

// TestAddressableColumn pins every case in which a DWARF column may not
// be encoded as-is.  The one that produced the reported defect is the
// last line of a file: DWARF reports a one-byte `}` line's epilogue at
// column 2, whose byte offset is past the whole table.
func TestAddressableColumn(t *testing.T) {
	// A three-line file: "ab", "", "}" (plus the trailing empty entry
	// `readSourceLineLengths` reports for a file ending in a newline).
	table := []uint32{2, 0, 1, 0}

	for _, tc := range []struct {
		name   string
		table  []uint32
		line   int64
		column int64
		want   int64
	}{
		{"in range", table, 1, 2, 2},
		{"column 1 on a one-byte line", table, 3, 1, 1},
		{"one past the end of a one-byte last line clamps", table, 3, 2, 1},
		{"one past the end of a longer line clamps", table, 1, 3, 2},
		{"far past the end clamps", table, 1, 99, 2},
		{"an empty line addresses nothing", table, 2, 1, 0},
		{"the phantom trailing entry addresses nothing", table, 4, 1, 0},
		{"a line beyond the table is refused", table, 5, 1, 0},
		{"no table at all is refused", nil, 1, 1, 0},
		{"empty table is refused", []uint32{}, 1, 1, 0},
		{"DWARF column 0 means no column", table, 1, 0, 0},
		{"a negative column is refused", table, 1, -1, 0},
		{"a zero line is refused", table, 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, addressableColumn(tc.table, tc.line, tc.column))
		})
	}
}
