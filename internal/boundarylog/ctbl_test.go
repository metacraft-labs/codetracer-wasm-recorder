package boundarylog

import (
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

// TestCTBLFramesDecodeExactlyAsTheProducerEncodesThem pins the decoder to the
// producer's own byte vectors. They are copied verbatim from
// `frames_are_encoded_exactly_as_the_spec_lays_them_out` in
// `codetracer/src/backend-manager/src/boundary_log.rs`; the encoder and this
// decoder live in different repositories and languages, so a layout change
// made on one side and not the other fails one of the two suites.
func TestCTBLFramesDecodeExactlyAsTheProducerEncodesThem(t *testing.T) {
	header := []byte{
		30, 0, 0, 0, 0x01, 1, 0, 0, 0, 'p', 1, 0, 0, 0, 1, 0, 0, 0, 'x', 1, 0, 0, 0,
		'/', 1, 0, 0, 0, 'r', 1, 0, 0, 0, '1',
	}
	frames := [][]byte{
		{9, 0, 0, 0, 0x02, 4, 0, 0, 0, 'a', '.', 'j', 's'},
		{13, 0, 0, 0, 0x04, 1, 0, 0, 0, 0xFE, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		{12, 0, 0, 0, 0x07, 3, 0, 0, 0, 0x01, 2, 0, 0, 0, '-', '7'},
		{3, 0, 0, 0, 0x06, 0x03, 1},
		{14, 0, 0, 0, 0x05, 2, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0x06},
		{14, 0, 0, 0, 0x09, 12, 0, 0, 0, 1, 0, 0, 0, 'm', 0, 0, 0, 0},
		{7, 0, 0, 0, 0x08, 2, 0, 0, 0, 0xC3, 0xA9},
		{1, 0, 0, 0, 0x0A},
	}
	log := append([]byte("CTBL\x01"), header...)
	for _, f := range frames {
		log = append(log, f...)
	}

	events, h, ended := DecodeTestLogEvents(t, log)
	require.True(t, ended)
	require.Equal(t, &logHeader{
		Program: "p", Args: []string{"x"}, Workdir: "/", RecorderName: "r", RecorderVersion: "1",
	}, h)

	path, name := "a.js", "é"
	require.Equal(t, []traceEvent{
		{Path: &path},
		{Step: &stepRecord{PathID: 1, Line: -2}},
		{Value: &valueRecord{VariableID: 3, Value: rawValue{Kind: "Int", Text: "-7"}}},
		{Return: &returnRecord{ReturnValue: rawValue{Kind: "Bool", Text: "true"}}},
		{Call: &callRecord{FunctionID: 2, Args: []argRecord{{VariableID: 0, Value: rawValue{Kind: "None"}}}}},
		{Event: &eventRecord{Kind: 12, Metadata: "m", Content: ""}},
		{VariableName: &name},
	}, events)

	// And the test-side encoder agrees with the same vectors.
	require.Equal(t, frames[0], EncodeTestRecord(map[string]any{"Path": "a.js"}))
	require.Equal(t, frames[1], EncodeTestRecord(map[string]any{
		"Step": map[string]any{"path_id": 1, "line": -2}}))
}

// TestCTBLRefusesAFrameWithTrailingBytes: a frame is consumed exactly, so a
// producer and a reader that disagree about a field's width are caught at
// the first record rather than drifting.
func TestCTBLRefusesAFrameWithTrailingBytes(t *testing.T) {
	log := EncodeTestLog("p", "/", nil, false)
	log = append(log, 10, 0, 0, 0, 0x02, 4, 0, 0, 0, 'a', '.', 'j', 's', 0)
	d := &frameDecoder{}
	d.feed(log)
	_, _, err := d.next()
	require.Error(t, err)
	require.Contains(t, err.Error(), "after its last field")
}
