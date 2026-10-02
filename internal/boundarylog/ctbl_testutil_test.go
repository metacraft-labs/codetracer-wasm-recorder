package boundarylog

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero/internal/boundarylog/ctbltest"
	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/testing/require"
)

// Test-side helpers over `ctbltest`, the independent CTBL encoder the tests
// describe recordings with. See that package for why it exists and what pins
// it.

// EncodeTestLog renders `events` as a CTBL v1 boundary log. `complete` adds
// the `End` frame a cleanly finished session ends with.
func EncodeTestLog(program, workdir string, events []map[string]any, complete bool) []byte {
	return ctbltest.Encode(ctbltest.DefaultHeader(program, workdir), events, complete)
}

// EncodeTestRecord renders one record as a frame.
func EncodeTestRecord(ev map[string]any) []byte { return ctbltest.EncodeRecord(ev) }

// WriteTestRecording writes `log` as the boundary log of a new CTFS
// container at `path`, through the canonical CTFS writer.
func WriteTestRecording(t *testing.T, path string, log []byte) {
	t.Helper()
	require.NoError(t, ctbltest.WriteRecording(path, log))
}

// ReadTestLog returns the boundary log stored in a `.ct`.
func ReadTestLog(t *testing.T, ctPath string) []byte {
	t.Helper()
	c, err := ctfs.Open(ctPath)
	require.NoError(t, err)
	log, err := c.ReadFile(BoundaryLogFileName)
	require.NoError(t, err)
	return log
}

// SplitTestLogFrames splits a boundary log into its prefix and its frames (each
// with its length prefix), and reports each frame's tag.
func SplitTestLogFrames(t *testing.T, log []byte) (prefix []byte, frames [][]byte, tags []byte) {
	t.Helper()
	require.True(t, len(log) >= ctblPrefixLen, "shorter than a boundary-log prefix")
	prefix = log[:ctblPrefixLen]
	for i := ctblPrefixLen; i < len(log); {
		require.True(t, i+4 <= len(log), "torn length prefix at %d", i)
		n := int(binary.LittleEndian.Uint32(log[i:]))
		require.True(t, i+4+n <= len(log), "torn frame at %d", i)
		frames = append(frames, log[i:i+4+n])
		tags = append(tags, log[i+4])
		i += 4 + n
	}
	return prefix, frames, tags
}

// DecodeTestLogEvents decodes a boundary log's records, as the assembler sees them.
func DecodeTestLogEvents(t *testing.T, log []byte) ([]traceEvent, *logHeader, bool) {
	t.Helper()
	d := &frameDecoder{}
	d.feed(log)
	var out []traceEvent
	for {
		ev, ok, err := d.next()
		require.NoError(t, err)
		if !ok {
			break
		}
		out = append(out, *ev)
	}
	return out, d.header, d.ended
}

// ReadTestLogFile returns the boundary log stored in a `.ct`, for tests that
// feed it to a `StreamReader` themselves.
func ReadTestLogFile(ctPath string) ([]byte, error) {
	c, err := ctfs.Open(ctPath)
	if err != nil {
		return nil, err
	}
	return c.ReadFile(BoundaryLogFileName)
}

// LoadTestStreamMetadata is the Recording a streaming replay of `ctPath`
// starts from: its boundary log's Header, and no crossings — what
// `StreamReader.ReadHeader` hands the CLI.
func LoadTestStreamMetadata(ctPath string) (*Recording, error) {
	log, err := ReadTestLogFile(ctPath)
	if err != nil {
		return nil, err
	}
	d := &frameDecoder{}
	d.feed(log)
	for d.header == nil {
		_, ok, err := d.next()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
	}
	return recordingFromHeader(d.header, ctPath), nil
}

// CopyTestRecording writes a copy of the `.ct` at `src` to `dst`, carrying
// over every frame of its boundary log except the `Event` records whose
// metadata contains every one of `dropIfContains` (none dropped when it is
// empty). It reports how many frames were dropped.
func CopyTestRecording(t *testing.T, src, dst string, dropIfContains ...string) (string, int) {
	t.Helper()
	prefix, frames, tags := SplitTestLogFrames(t, ReadTestLog(t, src))
	out := append([]byte(nil), prefix...)
	dropped := 0
	for i, f := range frames {
		if len(dropIfContains) > 0 && tags[i] == tagEvent {
			p := &payloadReader{b: f[4:]}
			p.u8()
			ev, err := decodeRecord(tagEvent, p)
			require.NoError(t, err)
			all := true
			for _, s := range dropIfContains {
				if !strings.Contains(ev.Event.Metadata, s) {
					all = false
				}
			}
			if all {
				dropped++
				continue
			}
		}
		out = append(out, f...)
	}
	WriteTestRecording(t, dst, out)
	return dst, dropped
}
