// Package ctbltest renders boundary logs (CTBL v1,
// `codetracer-specs/Recording-Backends/Browser-Recording-Container.md` §3)
// for tests, and takes them apart again.
//
// Tests describe a recording as the record sequence the producer emits —
// `{"Path": …}`, `{"Step": {"path_id", "line"}}`, `{"Value": {"variable_id",
// "value": {"kind", "i"|"f"|"r"|"text"|"b"}}}` — because that is the
// vocabulary `codetracer/src/backend-manager/src/browser_stream_host.rs`
// translates browser events into, and because a test that perturbs a
// recording ("the export returned 621, not 620") says so most plainly in it.
// `Encode` turns such a sequence into the bytes the producer writes; `Decode`
// is its inverse. Nothing outside tests imports this package.
//
// It is deliberately a second, independent implementation of the format: it
// does not import `internal/boundarylog`, so a decoder bug cannot be
// mirrored by an encoder bug. Both are pinned to the producer's byte vectors
// (`TestCTBLFramesDecodeExactlyAsTheProducerEncodesThem` in `internal/boundarylog`).
package ctbltest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/tetratelabs/wazero/internal/ctfs"
	"github.com/tetratelabs/wazero/internal/ctfsffi"
)

// LogFileName is the CTFS internal file a recording's boundary log lives in.
const LogFileName = "boundary.log"

// Record tags, as the format numbers them.
const (
	TagHeader       = 0x01
	TagPath         = 0x02
	TagFunction     = 0x03
	TagStep         = 0x04
	TagCall         = 0x05
	TagReturn       = 0x06
	TagValue        = 0x07
	TagVariableName = 0x08
	TagEvent        = 0x09
	TagEnd          = 0x0A
)

// Header is a log's Header frame.
type Header struct {
	Program, Workdir, RecorderName, RecorderVersion string
	Args                                            []string
}

// DefaultHeader is the header `record-web` writes for a browser session of
// `program`.
func DefaultHeader(program, workdir string) Header {
	return Header{
		Program: program, Workdir: workdir,
		RecorderName: "codetracer-js-recorder-browser", RecorderVersion: "0.1.0",
	}
}

// Encode renders a boundary log. `complete` adds the End frame a cleanly
// finished session ends with.
func Encode(h Header, records []map[string]any, complete bool) []byte {
	out := []byte("CTBL\x01")
	out = append(out, frame(func(p *payload) {
		p.u8(TagHeader)
		p.str(h.Program)
		p.u32(uint32(len(h.Args)))
		for _, a := range h.Args {
			p.str(a)
		}
		p.str(h.Workdir)
		p.str(h.RecorderName)
		p.str(h.RecorderVersion)
	})...)
	for _, r := range records {
		out = append(out, EncodeRecord(r)...)
	}
	if complete {
		out = append(out, EndFrame()...)
	}
	return out
}

// EndFrame is the frame that ends a complete log.
func EndFrame() []byte { return frame(func(p *payload) { p.u8(TagEnd) }) }

// EncodeRecord renders one record as a frame.
func EncodeRecord(ev map[string]any) []byte {
	if len(ev) != 1 {
		panic(fmt.Sprintf("a record has exactly one kind: %v", ev))
	}
	for kind, body := range ev {
		return frame(func(p *payload) {
			switch kind {
			case "Path":
				p.u8(TagPath)
				p.str(body.(string))
			case "VariableName":
				p.u8(TagVariableName)
				p.str(body.(string))
			case "Step":
				m := body.(map[string]any)
				p.u8(TagStep)
				p.u32(uint32(num(m["path_id"])))
				p.i64(int64(num(m["line"])))
			case "Function":
				m := body.(map[string]any)
				p.u8(TagFunction)
				p.str(m["name"].(string))
				p.u32(uint32(num(m["path_id"])))
				p.i64(int64(num(m["line"])))
			case "Call":
				m := body.(map[string]any)
				p.u8(TagCall)
				p.u32(uint32(num(m["function_id"])))
				args, _ := m["args"].([]any)
				p.u32(uint32(len(args)))
				for _, a := range args {
					am := a.(map[string]any)
					p.u32(uint32(num(am["variable_id"])))
					p.value(am["value"].(map[string]any))
				}
			case "Return":
				p.u8(TagReturn)
				p.value(body.(map[string]any)["return_value"].(map[string]any))
			case "Value":
				m := body.(map[string]any)
				p.u8(TagValue)
				p.u32(uint32(num(m["variable_id"])))
				p.value(m["value"].(map[string]any))
			case "Event":
				m := body.(map[string]any)
				p.u8(TagEvent)
				p.u32(uint32(int32(num(m["kind"]))))
				p.str(m["metadata"].(string))
				p.str(m["content"].(string))
			default:
				panic("unknown record kind " + kind)
			}
		})
	}
	panic("unreachable")
}

// Decode is Encode's inverse: the header, the records in the same
// description, and whether the log ended with End. It fails on a torn frame.
func Decode(log []byte) (Header, []map[string]any, bool, error) {
	var h Header
	if len(log) < 5 || string(log[:4]) != "CTBL" || log[4] != 1 {
		return h, nil, false, fmt.Errorf("not a CTBL v1 boundary log")
	}
	frames, err := Frames(log)
	if err != nil {
		return h, nil, false, err
	}
	var out []map[string]any
	ended := false
	for _, f := range frames {
		r := &reader{b: f[4:]}
		switch tag := r.u8(); tag {
		case TagHeader:
			h.Program = r.str()
			n := r.u32()
			for i := uint32(0); i < n; i++ {
				h.Args = append(h.Args, r.str())
			}
			h.Workdir, h.RecorderName, h.RecorderVersion = r.str(), r.str(), r.str()
		case TagEnd:
			ended = true
		case TagPath:
			out = append(out, map[string]any{"Path": r.str()})
		case TagVariableName:
			out = append(out, map[string]any{"VariableName": r.str()})
		case TagStep:
			out = append(out, map[string]any{"Step": map[string]any{
				"path_id": float64(r.u32()), "line": float64(r.i64())}})
		case TagFunction:
			name := r.str()
			out = append(out, map[string]any{"Function": map[string]any{
				"name": name, "path_id": float64(r.u32()), "line": float64(r.i64())}})
		case TagCall:
			fid := r.u32()
			n := r.u32()
			args := []any{}
			for i := uint32(0); i < n; i++ {
				vid := r.u32()
				args = append(args, map[string]any{"variable_id": float64(vid), "value": r.value()})
			}
			out = append(out, map[string]any{"Call": map[string]any{
				"function_id": float64(fid), "args": args}})
		case TagReturn:
			out = append(out, map[string]any{"Return": map[string]any{"return_value": r.value()}})
		case TagValue:
			vid := r.u32()
			out = append(out, map[string]any{"Value": map[string]any{
				"variable_id": float64(vid), "value": r.value()}})
		case TagEvent:
			kind := int32(r.u32())
			meta := r.str()
			out = append(out, map[string]any{"Event": map[string]any{
				"kind": float64(kind), "metadata": meta, "content": r.str()}})
		default:
			return h, nil, false, fmt.Errorf("unknown record tag 0x%02x", tag)
		}
	}
	return h, out, ended, nil
}

// Frames splits a log, after its five-byte prefix, into whole frames (each
// with its length prefix). It fails on a torn frame.
func Frames(log []byte) ([][]byte, error) {
	var frames [][]byte
	for i := 5; i < len(log); {
		if i+4 > len(log) {
			return nil, fmt.Errorf("torn length prefix at %d", i)
		}
		n := int(binary.LittleEndian.Uint32(log[i:]))
		if i+4+n > len(log) {
			return nil, fmt.Errorf("torn frame at %d", i)
		}
		frames = append(frames, log[i:i+4+n])
		i += 4 + n
	}
	return frames, nil
}

// ReadLog returns the boundary log stored in the `.ct` at `path`.
func ReadLog(path string) ([]byte, error) {
	c, err := ctfs.Open(path)
	if err != nil {
		return nil, err
	}
	return c.ReadFile(LogFileName)
}

// WriteRecording writes `log` as the boundary log of a new CTFS container at
// `path`, through the canonical CTFS writer.
func WriteRecording(path string, log []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	_ = os.Remove(path)
	if err := ctfsffi.Create(path, 4096); err != nil {
		return err
	}
	return ctfsffi.Append(path, map[string][]byte{LogFileName: log})
}

// Rewrite copies the recording at `src` to `dst`, passing its decoded
// records through `edit`, and keeping its header and completeness.
func Rewrite(src, dst string, edit func([]map[string]any) []map[string]any) error {
	log, err := ReadLog(src)
	if err != nil {
		return err
	}
	h, records, ended, err := Decode(log)
	if err != nil {
		return err
	}
	return WriteRecording(dst, Encode(h, edit(records), ended))
}

// JSON renders one record in its description, for textual perturbation and
// for diagnostics.
func JSON(r map[string]any) string {
	b, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// FromJSON parses a record back from JSON.
func FromJSON(s string) map[string]any {
	var r map[string]any
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		panic(err)
	}
	return r
}

func num(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case uint32:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	panic(fmt.Sprintf("not a number: %#v", v))
}

type payload struct{ b []byte }

func (p *payload) u8(v byte)    { p.b = append(p.b, v) }
func (p *payload) u32(v uint32) { p.b = binary.LittleEndian.AppendUint32(p.b, v) }
func (p *payload) i64(v int64)  { p.b = binary.LittleEndian.AppendUint64(p.b, uint64(v)) }
func (p *payload) str(s string) {
	p.u32(uint32(len(s)))
	p.b = append(p.b, s...)
}

func (p *payload) value(v map[string]any) {
	text := func(key string) string {
		switch t := v[key].(type) {
		case string:
			return t
		case float64:
			return strconv.FormatFloat(t, 'g', -1, 64)
		default:
			return fmt.Sprintf("%v", t)
		}
	}
	switch v["kind"] {
	case "Int":
		p.u8(0x01)
		p.str(text("i"))
	case "Float":
		p.u8(0x02)
		p.str(text("f"))
	case "Bool":
		p.u8(0x03)
		if b, _ := v["b"].(bool); b {
			p.u8(1)
		} else {
			p.u8(0)
		}
	case "String":
		p.u8(0x04)
		p.str(text("text"))
	case "Raw":
		p.u8(0x05)
		p.str(text("r"))
	case "None":
		p.u8(0x06)
	default:
		panic(fmt.Sprintf("unknown value kind %v", v["kind"]))
	}
}

func frame(fill func(*payload)) []byte {
	p := &payload{}
	fill(p)
	out := binary.LittleEndian.AppendUint32(nil, uint32(len(p.b)))
	return append(out, p.b...)
}

type reader struct {
	b []byte
	i int
}

func (r *reader) u8() byte { r.i++; return r.b[r.i-1] }
func (r *reader) u32() uint32 {
	r.i += 4
	return binary.LittleEndian.Uint32(r.b[r.i-4:])
}
func (r *reader) i64() int64 {
	r.i += 8
	return int64(binary.LittleEndian.Uint64(r.b[r.i-8:]))
}
func (r *reader) str() string {
	n := int(r.u32())
	r.i += n
	return string(r.b[r.i-n : r.i])
}
func (r *reader) value() map[string]any {
	switch r.u8() {
	case 0x01:
		return map[string]any{"kind": "Int", "i": r.str(), "type_id": float64(0)}
	case 0x02:
		return map[string]any{"kind": "Float", "f": r.str(), "type_id": float64(0)}
	case 0x03:
		return map[string]any{"kind": "Bool", "b": r.u8() != 0, "type_id": float64(0)}
	case 0x04:
		return map[string]any{"kind": "String", "text": r.str(), "type_id": float64(0)}
	case 0x05:
		return map[string]any{"kind": "Raw", "r": r.str(), "type_id": float64(0)}
	default:
		return map[string]any{"kind": "None", "type_id": float64(0)}
	}
}
