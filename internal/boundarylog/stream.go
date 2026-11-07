package boundarylog

// Incremental reading of a boundary recording that is still being produced —
// `WASM-Replay-Snapshots-And-Slices.md` §2.
//
// `LoadRecording` reads a finished `boundary.log` in one gulp. That is fine for
// a recording that is over and useless for one that is not, and §2 is explicit
// that snapshots are "derived **continuously, during recording**, not as a
// separate pass afterwards. … When the page stops, the snapshots are already
// there."
//
// This file supplies the missing half: a reader that consumes the same bytes as
// they arrive and yields **call groups** (one top-level exported call plus every
// crossing nested inside it) the moment each is complete, so the replay driver
// can execute it and emit a snapshot at the quiescent point that follows —
// while the browser is still recording.
//
// # What it reads
//
// The CTBL v1 boundary log (`ctbl.go`, and
// `Browser-Recording-Container.md` §3), exactly as `record-web` writes it to
// its `--snapshot-consumer`'s stdin: the magic and version, the `Header` frame,
// one frame per record, and an `End` frame when the session finished cleanly.
// Every frame is length-prefixed, so a frame is decoded only once all of it has
// arrived and a half-written one is never handed to the assembler.
//
// # Where the bytes come from
//
// `NewStreamReader` takes an `io.Reader` whose `io.EOF` means "the producer has
// finished", which is exactly what the pipe from the `record-web` daemon gives.
//
// Backpressure falls out of that contract rather than being bolted onto it:
// `NextGroup` reads only when it needs another group, and the replay driver
// calls it only between exported calls, so a producer writing into a pipe
// blocks as soon as the pipe fills. The replayer never accumulates an unbounded
// backlog of unreplayed crossings.

import (
	"errors"
	"fmt"
	"io"
)

// TruncationKind classifies how a boundary stream ended badly.
type TruncationKind int

const (
	// TruncatedMidRecord: the stream stopped inside a frame.
	TruncatedMidRecord TruncationKind = iota
	// TruncatedMidCrossing: the stream ended with a crossing still open.
	TruncatedMidCrossing
	// TruncatedUnterminated: every frame was whole, but the `End` frame
	// never came.
	TruncatedUnterminated
)

func (k TruncationKind) String() string {
	switch k {
	case TruncatedMidRecord:
		return "mid-record"
	case TruncatedMidCrossing:
		return "mid-crossing"
	default:
		return "unterminated"
	}
}

// TruncationError reports that a streamed boundary recording ended before the
// producer finished writing it.
//
// It is deliberately not one undifferentiated "truncated" error. The three
// kinds mean materially different things to whoever is holding the pieces:
//
//   - `TruncatedUnterminated` is the *benign* one. Every crossing the stream
//     carried was complete, so every call group already replayed is faithful
//     and every snapshot already emitted is valid. A recording of a page that
//     was killed rather than unloaded lands here, and what it has is worth
//     keeping.
//   - `TruncatedMidCrossing` means an exported call was entered and never
//     returned. The crossings before it are still faithful; the open one is
//     not, and is dropped.
//   - `TruncatedMidRecord` means the last bytes are not a whole frame. The
//     producer writes each frame with one write, so this means the bytes were
//     cut by something other than the producer stopping between records.
//
// `Groups` reports how many complete call groups were replayed before the
// stream ended, so a caller can say what survived rather than only what failed.
type TruncationError struct {
	Kind TruncationKind
	// Groups is the number of complete call groups replayed before the end.
	Groups int
	// OpenCrossings is the number of crossings left open at the end.
	OpenCrossings int
	// PendingBytes is the size of the incomplete trailing frame, if any.
	PendingBytes int
}

func (e *TruncationError) Error() string {
	switch e.Kind {
	case TruncatedMidRecord:
		return fmt.Sprintf(
			"the boundary stream ended in the middle of a frame: %d byte(s) of an "+
				"unfinished frame were buffered after %d complete exported call(s). "+
				"The trailing bytes cannot be interpreted",
			e.PendingBytes, e.Groups)
	case TruncatedMidCrossing:
		return fmt.Sprintf(
			"the boundary stream ended with %d boundary crossing(s) still open, after "+
				"%d complete exported call(s). An exported call was entered and never "+
				"returned, so it cannot be replayed faithfully (spec §8)",
			e.OpenCrossings, e.Groups)
	default:
		return fmt.Sprintf(
			"the boundary stream ended without its End frame, after %d complete "+
				"exported call(s). Every crossing it did carry was complete, so what "+
				"was already replayed is faithful — but the recording is a prefix, "+
				"not the whole page's execution",
			e.Groups)
	}
}

// IsTruncation reports whether err is a `*TruncationError`, and of which kind.
func IsTruncation(err error) (*TruncationError, bool) {
	var t *TruncationError
	if errors.As(err, &t) {
		return t, true
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// StreamReader
// ---------------------------------------------------------------------------

// streamChunk is the read size. It is large enough that a normal record
// arrives in one read and small enough that a slow replayer's backpressure
// reaches the producer promptly.
const streamChunk = 32 * 1024

// StreamReader yields a boundary recording's call groups as they arrive.
type StreamReader struct {
	src io.Reader
	dec *frameDecoder
	asm *assembler

	chunk []byte
	// next is the index of the next group to hand out.
	next int
	// eof is set once the source has returned io.EOF.
	eof bool
	// closed is set once the end-of-stream checks have run.
	closed bool
	// failed is the first error, returned again on every later call.
	failed error
}

// NewStreamReader reads a CTBL boundary log from `r`.
//
// `r` must block while the producer is alive and return `io.EOF` only once it
// has finished — the contract a pipe already has. Returning EOF early would be
// indistinguishable from the recording ending, and would truncate it.
func NewStreamReader(r io.Reader) *StreamReader {
	return &StreamReader{
		src:   r,
		dec:   &frameDecoder{},
		asm:   newAssembler(),
		chunk: make([]byte, streamChunk),
	}
}

// ReadHeader blocks until the stream's `Header` frame has arrived and returns
// a Recording carrying its metadata and no crossings — what `StreamingReplay`
// wants as `Options.Recording`. `source` names the stream in diagnostics.
//
// The daemon writes the header the moment it spawns its consumer, so this
// returns as soon as the page has announced itself, long before the first
// exported call.
func (s *StreamReader) ReadHeader(source string) (*Recording, error) {
	for s.dec.header == nil {
		if s.failed != nil {
			return nil, s.failed
		}
		if s.eof {
			return nil, s.fail(fmt.Errorf(
				"the boundary stream ended before its Header frame arrived "+
					"(%d byte(s) received)", s.dec.pending()))
		}
		if err := s.pump(); err != nil {
			return nil, s.fail(err)
		}
	}
	return recordingFromHeader(s.dec.header, source), nil
}

// NextGroup returns the next complete call group: one top-level exported call
// together with every crossing nested inside it.
//
// It blocks until the group is complete or the producer finishes. At a clean
// end of stream it returns `io.EOF`; at an unclean one, a `*TruncationError`
// naming what survived.
func (s *StreamReader) NextGroup() ([]Crossing, error) {
	if s.failed != nil {
		return nil, s.failed
	}
	for {
		if s.asm.groups() > s.next {
			g := s.asm.group(s.next)
			s.next++
			return g, nil
		}
		if s.eof {
			if !s.closed {
				s.closed = true
				if err := s.finishStream(); err != nil {
					return nil, s.fail(err)
				}
				continue
			}
			return nil, io.EOF
		}
		if err := s.pump(); err != nil {
			return nil, s.fail(err)
		}
	}
}

// GroupsRead reports how many call groups have been handed out.
func (s *StreamReader) GroupsRead() int { return s.next }

// pump reads one chunk from the producer and feeds whatever it completes into
// the assembler.
func (s *StreamReader) pump() error {
	n, err := s.src.Read(s.chunk)
	if n > 0 {
		s.dec.feed(s.chunk[:n])
		if derr := s.drain(); derr != nil {
			return derr
		}
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, io.EOF):
		s.eof = true
		return nil
	default:
		return fmt.Errorf("reading the boundary stream: %w", err)
	}
}

// drain decodes every complete record the decoder can now produce.
func (s *StreamReader) drain() error {
	for {
		ev, ok, err := s.dec.next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := s.asm.push(ev); err != nil {
			return fmt.Errorf("recovering boundary crossings from the stream: %w", err)
		}
	}
}

// finishStream closes the assembler and classifies an unclean end.
//
// The order of the checks is the order of severity: bytes that are not a whole
// frame, then a crossing left open, then a stream that never sent `End`. Each
// reports how many complete call groups were already replayed, because that
// number is what survives — every one of them was driven from complete
// crossings and every snapshot taken after one is valid.
func (s *StreamReader) finishStream() error {
	pending := s.dec.pending()
	open := s.asm.openCrossings()
	if pending > 0 {
		return &TruncationError{
			Kind: TruncatedMidRecord, Groups: s.next,
			OpenCrossings: open, PendingBytes: pending,
		}
	}
	if err := s.asm.finish(); err != nil {
		if open > 0 {
			return &TruncationError{
				Kind: TruncatedMidCrossing, Groups: s.next, OpenCrossings: open,
			}
		}
		return err
	}
	if !s.dec.ended {
		return &TruncationError{Kind: TruncatedUnterminated, Groups: s.next}
	}
	return nil
}

func (s *StreamReader) fail(err error) error {
	s.failed = err
	return err
}

// HostState returns the spec §3.3 / §3.4 state the stream has carried so
// far, or nil if it has carried none (M44b).
//
// It is a live pointer into the reader's assembler, not a copy: a §3.4
// mutation that arrives later is appended to the very object a caller
// already holds. That is what lets `StreamingReplay` hand it to the
// replayer once, at instantiation, and have every later mutation reach
// `applyMutations` without any further plumbing — the replayer re-reads
// `Recording.HostState` on every import call.
//
// It reports only what has ARRIVED. Before the first call group is
// complete that is usually nil, which is why `StreamingReplay` defers
// instantiation until then: the §3.3 record is the last thing the
// producer sends before the first `Call`.
func (s *StreamReader) HostState() *HostState { return s.asm.hostState }
