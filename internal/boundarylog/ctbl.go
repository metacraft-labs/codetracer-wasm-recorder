package boundarylog

import (
	"encoding/binary"
	"fmt"
	"unicode/utf8"
)

// This file decodes CTBL v1, the boundary log's binary encoding, specified by
// `codetracer-specs/Recording-Backends/Browser-Recording-Container.md` §3.
//
// It is the only CTBL decoder anywhere: the producer
// (`codetracer/src/backend-manager/src/boundary_log.rs`) writes it and never
// reads it, and CodeTracer does not open a boundary log as a recording. The
// byte vectors `TestCTBLFramesDecodeExactlyAsTheProducerEncodesThem` pins are
// the producer's own test vectors, so a layout change on either side fails one
// of the two suites.
//
//	stream  := "CTBL" version:u8 frame*
//	frame   := len:u32 payload[len]
//	payload := tag:u8 body
//	str     := len:u32 utf8[len]
//	value   := vtag:u8 body
//
// All integers are little-endian.

// BoundaryLogFileName is the CTFS internal file a `.ct` stores its boundary
// log under. Twelve characters: the CTFS internal-name limit.
const BoundaryLogFileName = "boundary.log"

const (
	ctblMagic   = "CTBL"
	ctblVersion = 1
	// ctblPrefixLen is the magic plus the version byte.
	ctblPrefixLen = 5
	// ctblMaxFrame bounds a single frame. A boundary-log record is one host
	// interaction; a length beyond this is a corrupted length prefix, and
	// buffering towards it would only delay the error.
	ctblMaxFrame = 256 << 20
)

const (
	tagHeader       = 0x01
	tagPath         = 0x02
	tagFunction     = 0x03
	tagStep         = 0x04
	tagCall         = 0x05
	tagReturn       = 0x06
	tagValue        = 0x07
	tagVariableName = 0x08
	tagEvent        = 0x09
	tagEnd          = 0x0A
)

const (
	vtagInt    = 0x01
	vtagFloat  = 0x02
	vtagBool   = 0x03
	vtagString = 0x04
	vtagRaw    = 0x05
	vtagNone   = 0x06
)

// logHeader is the `Header` frame: who produced the recording and for what.
type logHeader struct {
	Program         string
	Args            []string
	Workdir         string
	RecorderName    string
	RecorderVersion string
}

// frameDecoder turns bytes into boundary-log records as they arrive.
//
// It is fed incrementally (`feed`) and yields one record per `next` call, so
// the same code serves a finished `boundary.log` and a stream a producer is
// still writing.
type frameDecoder struct {
	buf []byte
	i   int

	prefixSeen bool
	header     *logHeader
	// ended is set once the `End` frame has been decoded.
	ended bool
}

func (d *frameDecoder) feed(b []byte) {
	d.compact()
	d.buf = append(d.buf, b...)
}

// compact drops the bytes of records already returned.
func (d *frameDecoder) compact() {
	if d.i == 0 {
		return
	}
	n := copy(d.buf, d.buf[d.i:])
	d.buf = d.buf[:n]
	d.i = 0
}

// pending is the number of buffered bytes that do not yet form a whole
// frame (or a whole prefix). Non-zero at end of input means the stream was
// torn inside a frame.
func (d *frameDecoder) pending() int { return len(d.buf) - d.i }

// next returns the next record, or ok=false when no complete record is
// buffered. The `Header` and `End` frames are consumed here rather than
// returned: the header is available as `d.header`, the end as `d.ended`.
func (d *frameDecoder) next() (ev *traceEvent, ok bool, err error) {
	for {
		if !d.prefixSeen {
			if d.pending() < ctblPrefixLen {
				if d.pending() > 0 && string(d.buf[d.i:]) != ctblMagic[:min(len(ctblMagic), d.pending())] {
					return nil, false, fmt.Errorf(
						"a boundary log must start with %q, but it starts with %q; "+
							"this is not a CTBL boundary log (a `trace.json` from the "+
							"retired JSON layout is refused by name)",
						ctblMagic, d.buf[d.i:])
				}
				return nil, false, nil
			}
			if string(d.buf[d.i:d.i+4]) != ctblMagic {
				return nil, false, fmt.Errorf(
					"a boundary log must start with %q, but it starts with %q; "+
						"this is not a CTBL boundary log", ctblMagic, d.buf[d.i:d.i+4])
			}
			if v := d.buf[d.i+4]; v != ctblVersion {
				return nil, false, fmt.Errorf(
					"boundary log version %d is not supported: this reader reads "+
						"version %d only", v, ctblVersion)
			}
			d.i += ctblPrefixLen
			d.prefixSeen = true
		}
		if d.pending() < 4 {
			return nil, false, nil
		}
		n := binary.LittleEndian.Uint32(d.buf[d.i:])
		if n == 0 || n > ctblMaxFrame {
			return nil, false, fmt.Errorf("boundary log frame of %d byte(s) is malformed", n)
		}
		if d.pending() < 4+int(n) {
			return nil, false, nil
		}
		payload := d.buf[d.i+4 : d.i+4+int(n)]
		d.i += 4 + int(n)

		if d.ended {
			return nil, false, fmt.Errorf("a boundary log carries a record after its End frame")
		}
		p := &payloadReader{b: payload}
		tag := p.u8()
		if d.header == nil && tag != tagHeader {
			return nil, false, fmt.Errorf(
				"a boundary log's first frame must be its Header, but it is tag 0x%02x", tag)
		}
		switch tag {
		case tagHeader:
			if d.header != nil {
				return nil, false, fmt.Errorf("a boundary log carries a second Header frame")
			}
			h := &logHeader{Program: p.str()}
			argc := p.u32()
			for j := uint32(0); j < argc && p.err == nil; j++ {
				h.Args = append(h.Args, p.str())
			}
			h.Workdir = p.str()
			h.RecorderName = p.str()
			h.RecorderVersion = p.str()
			if err := p.done(); err != nil {
				return nil, false, err
			}
			d.header = h
			continue
		case tagEnd:
			if err := p.done(); err != nil {
				return nil, false, err
			}
			d.ended = true
			continue
		}
		ev, err := decodeRecord(tag, p)
		if err != nil {
			return nil, false, err
		}
		return ev, true, nil
	}
}

func decodeRecord(tag byte, p *payloadReader) (*traceEvent, error) {
	ev := &traceEvent{}
	switch tag {
	case tagPath:
		s := p.str()
		ev.Path = &s
	case tagFunction:
		ev.Function = &functionRecord{Name: p.str(), PathID: p.u32(), Line: p.i64()}
	case tagStep:
		ev.Step = &stepRecord{PathID: p.u32(), Line: p.i64()}
	case tagCall:
		c := &callRecord{FunctionID: p.u32()}
		argc := p.u32()
		for j := uint32(0); j < argc && p.err == nil; j++ {
			c.Args = append(c.Args, argRecord{VariableID: p.u32(), Value: p.value()})
		}
		ev.Call = c
	case tagReturn:
		ev.Return = &returnRecord{ReturnValue: p.value()}
	case tagValue:
		ev.Value = &valueRecord{VariableID: p.u32(), Value: p.value()}
	case tagVariableName:
		s := p.str()
		ev.VariableName = &s
	case tagEvent:
		ev.Event = &eventRecord{Kind: int(p.i32()), Metadata: p.str(), Content: p.str()}
	default:
		return nil, fmt.Errorf("boundary log record tag 0x%02x is not one this reader knows", tag)
	}
	if err := p.done(); err != nil {
		return nil, err
	}
	return ev, nil
}

// payloadReader reads one frame's payload. The first error sticks; `done`
// reports it, or bytes left over, since a frame must be consumed exactly.
type payloadReader struct {
	b   []byte
	i   int
	err error
}

func (p *payloadReader) take(n int) []byte {
	if p.err != nil {
		return nil
	}
	if n < 0 || p.i+n > len(p.b) {
		p.err = fmt.Errorf("boundary log frame is shorter than its fields: wanted %d byte(s) at offset %d of %d", n, p.i, len(p.b))
		return nil
	}
	s := p.b[p.i : p.i+n]
	p.i += n
	return s
}

func (p *payloadReader) u8() byte {
	if b := p.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (p *payloadReader) u32() uint32 {
	if b := p.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (p *payloadReader) i32() int32 { return int32(p.u32()) }

func (p *payloadReader) i64() int64 {
	if b := p.take(8); b != nil {
		return int64(binary.LittleEndian.Uint64(b))
	}
	return 0
}

func (p *payloadReader) str() string {
	n := p.u32()
	b := p.take(int(n))
	if b == nil {
		return ""
	}
	if !utf8.Valid(b) {
		p.err = fmt.Errorf("boundary log string at offset %d is not UTF-8", p.i-len(b))
		return ""
	}
	return string(b)
}

func (p *payloadReader) value() rawValue {
	switch vtag := p.u8(); vtag {
	case vtagInt:
		return rawValue{Kind: "Int", Text: p.str()}
	case vtagFloat:
		return rawValue{Kind: "Float", Text: p.str()}
	case vtagBool:
		if p.u8() != 0 {
			return rawValue{Kind: "Bool", Text: "true"}
		}
		return rawValue{Kind: "Bool", Text: "false"}
	case vtagString:
		return rawValue{Kind: "String", Text: p.str()}
	case vtagRaw:
		return rawValue{Kind: "Raw", Text: p.str()}
	case vtagNone:
		return rawValue{Kind: "None"}
	default:
		if p.err == nil {
			p.err = fmt.Errorf("boundary log value tag 0x%02x is not one this reader knows", vtag)
		}
		return rawValue{}
	}
}

func (p *payloadReader) done() error {
	if p.err != nil {
		return p.err
	}
	if p.i != len(p.b) {
		return fmt.Errorf("boundary log frame has %d byte(s) after its last field", len(p.b)-p.i)
	}
	return nil
}
