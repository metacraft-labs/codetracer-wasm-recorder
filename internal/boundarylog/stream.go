package boundarylog

// Incremental reading of a boundary recording that is still being produced —
// `WASM-Replay-Snapshots-And-Slices.md` §2.
//
// `LoadRecording` reads a finished `trace.json` in one gulp: `os.ReadFile` then
// `json.Unmarshal` of the whole array. That is fine for a recording that is
// over and useless for one that is not, and §2 is explicit that snapshots are
// "derived **continuously, during recording**, not as a separate pass
// afterwards. … When the page stops, the snapshots are already there."
//
// This file supplies the missing half: a reader that consumes the same bytes as
// they arrive and yields **call groups** (one top-level exported call plus every
// crossing nested inside it) the moment each is complete, so the replay driver
// can execute it and emit a snapshot at the quiescent point that follows —
// while the browser is still recording.
//
// # What it reads
//
// The producer (`browser_stream_host.rs`'s `JsonFileCtfsWriter`) writes a JSON
// array of records. Streaming therefore needs to split a *growing* JSON array
// into its elements without ever handing a half-written one to the decoder,
// which `encoding/json`'s `Decoder` cannot do — a failed `Decode` leaves it
// unusable, so "try and retry when more arrives" is not available. The scanner
// below does the split itself: it tracks brace depth and string escaping and
// only emits an element once its closing brace has arrived.
//
// # Where the bytes come from
//
// `NewStreamReader` takes an `io.Reader` whose `io.EOF` means "the producer has
// finished", which is exactly what a pipe or socket from the `record-web`
// daemon gives. `FollowFile` adapts the other shape — a file the daemon is
// appending to — into the same contract, by blocking at end-of-file until a
// caller-supplied channel says the producer has stopped.
//
// Backpressure falls out of that contract rather than being bolted onto it:
// `NextGroup` reads only when it needs another group, and the replay driver
// calls it only between exported calls, so a producer writing into a pipe
// blocks as soon as the pipe fills. The replayer never accumulates an unbounded
// backlog of unreplayed crossings.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// TruncationKind classifies how a boundary stream ended badly.
type TruncationKind int

const (
	// TruncatedMidRecord means the stream ended in the middle of a JSON
	// record. The producer died mid-write; the trailing bytes are unusable.
	TruncatedMidRecord TruncationKind = iota
	// TruncatedMidCrossing means the records were all complete but a boundary
	// crossing was still open — an exported call that never returned.
	TruncatedMidCrossing
	// TruncatedUnterminated means every record and every crossing was
	// complete, but the JSON array was never closed: the producer stopped
	// cleanly between calls without finishing the document.
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
//   - `TruncatedMidRecord` means the last bytes are not a record at all.
//
// `Groups` reports how many complete call groups were replayed before the
// stream ended, so a caller can say what survived rather than only what failed.
type TruncationError struct {
	Kind TruncationKind
	// Groups is the number of complete call groups yielded before the end.
	Groups int
	// OpenCrossings is how many crossings were still open.
	OpenCrossings int
	// PendingBytes is how many bytes of a partial record were buffered.
	PendingBytes int
}

func (e *TruncationError) Error() string {
	switch e.Kind {
	case TruncatedMidRecord:
		return fmt.Sprintf(
			"the boundary stream ended in the middle of a record: %d byte(s) of an "+
				"unfinished JSON object were buffered after %d complete exported "+
				"call(s). The producer stopped mid-write, so the trailing bytes cannot "+
				"be interpreted",
			e.PendingBytes, e.Groups)
	case TruncatedMidCrossing:
		return fmt.Sprintf(
			"the boundary stream ended with %d boundary crossing(s) still open, after "+
				"%d complete exported call(s). An exported call was entered and never "+
				"returned, so it cannot be replayed faithfully (spec §8)",
			e.OpenCrossings, e.Groups)
	default:
		return fmt.Sprintf(
			"the boundary stream ended without closing its JSON array, after %d "+
				"complete exported call(s). Every crossing it did carry was complete, "+
				"so what was already replayed is faithful — but the recording is a "+
				"prefix, not the whole page's execution",
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
// Splitting a growing JSON array
// ---------------------------------------------------------------------------

// jsonArrayScanner splits a JSON array into its top-level elements as the bytes
// arrive.
//
// It accepts exactly the shape the producer writes: an array whose elements are
// objects. A non-object element is refused rather than guessed at — the
// alternative, a general JSON value scanner, would add number/literal
// termination rules for a case that cannot occur and could not be tested
// against a real producer.
type jsonArrayScanner struct {
	buf []byte
	// i is the cursor: the next byte to examine.
	i int
	// started and done record whether the opening `[` and closing `]` have
	// been seen.
	started, done bool
	// elemStart is the index in buf at which the element being scanned begins,
	// or -1 between elements.
	elemStart int
	// depth, inString and escaped are the object-scanning state.
	depth    int
	inString bool
	escaped  bool
}

func newJSONArrayScanner() *jsonArrayScanner {
	return &jsonArrayScanner{elemStart: -1}
}

func (s *jsonArrayScanner) feed(b []byte) { s.buf = append(s.buf, b...) }

// pending reports how many buffered bytes belong to an element that has not
// finished arriving.
func (s *jsonArrayScanner) pending() int {
	if s.elemStart < 0 {
		return 0
	}
	return len(s.buf) - s.elemStart
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// next returns the next complete element, or ok=false when more bytes are
// needed (or the array has closed).
func (s *jsonArrayScanner) next() (elem []byte, ok bool, err error) {
	for {
		if !s.started {
			for s.i < len(s.buf) && isJSONSpace(s.buf[s.i]) {
				s.i++
			}
			if s.i >= len(s.buf) {
				return nil, false, nil
			}
			if s.buf[s.i] != '[' {
				return nil, false, fmt.Errorf(
					"a boundary stream must be a JSON array of records, but it starts "+
						"with %q", string(s.buf[s.i]))
			}
			s.started = true
			s.i++
			continue
		}

		if s.elemStart < 0 {
			// Between elements: skip separators, and stop at the array's end.
			for s.i < len(s.buf) && (isJSONSpace(s.buf[s.i]) || s.buf[s.i] == ',') {
				s.i++
			}
			s.compact()
			if s.i >= len(s.buf) {
				return nil, false, nil
			}
			if s.done {
				return nil, false, fmt.Errorf(
					"a boundary stream carries %q after its JSON array closed",
					string(s.buf[s.i]))
			}
			if s.buf[s.i] == ']' {
				s.done = true
				s.i++
				return nil, false, nil
			}
			if s.buf[s.i] != '{' {
				return nil, false, fmt.Errorf(
					"a boundary stream's records must be JSON objects, but one starts "+
						"with %q", string(s.buf[s.i]))
			}
			s.elemStart = s.i
			s.depth, s.inString, s.escaped = 0, false, false
		}

		for s.i < len(s.buf) {
			c := s.buf[s.i]
			s.i++
			if s.inString {
				switch {
				case s.escaped:
					s.escaped = false
				case c == '\\':
					s.escaped = true
				case c == '"':
					s.inString = false
				}
				continue
			}
			switch c {
			case '"':
				s.inString = true
			case '{':
				s.depth++
			case '}':
				s.depth--
				if s.depth == 0 {
					elem = s.buf[s.elemStart:s.i]
					s.elemStart = -1
					return elem, true, nil
				}
			}
		}
		return nil, false, nil
	}
}

// compact drops the bytes already consumed, so following a long recording does
// not grow the buffer without bound. It is only safe between elements, where no
// slice into the buffer is outstanding.
func (s *jsonArrayScanner) compact() {
	if s.elemStart >= 0 || s.i == 0 {
		return
	}
	n := copy(s.buf, s.buf[s.i:])
	s.buf = s.buf[:n]
	s.i = 0
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
	sc  *jsonArrayScanner
	asm *assembler

	chunk []byte
	// next is the index of the group `NextGroup` will hand out.
	next int
	// eof records that the producer has finished.
	eof bool
	// closed records that `asm.finish` has already run.
	closed bool
	// failed holds a terminal error, so a second call reports the same thing.
	failed error
}

// NewStreamReader reads a boundary recording's `trace.json` from `r`.
//
// `r` must block while the producer is alive and return `io.EOF` only once it
// has finished — the contract a pipe or socket already has, and the one
// `FollowFile` gives a growing file. Returning EOF early would be
// indistinguishable from the recording ending, and would truncate it.
func NewStreamReader(r io.Reader) *StreamReader {
	return &StreamReader{
		src:   r,
		sc:    newJSONArrayScanner(),
		asm:   newAssembler(),
		chunk: make([]byte, streamChunk),
	}
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
				// `finish` can complete one last group (a trailing crossing
				// that no `Return` closed on its own), so go round again.
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
		s.sc.feed(s.chunk[:n])
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

// drain decodes every complete record the scanner can now produce.
func (s *StreamReader) drain() error {
	for {
		raw, ok, err := s.sc.next()
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		var ev traceEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			return fmt.Errorf("decoding a boundary stream record: %w", err)
		}
		if err := s.asm.push(&ev); err != nil {
			return fmt.Errorf("recovering boundary crossings from the stream: %w", err)
		}
	}
}

// finishStream closes the assembler and classifies an unclean end.
//
// The order of the checks is the order of severity: bytes that are not a record
// at all, then a crossing left open, then an array that never closed. Each
// reports how many complete call groups were already replayed, because that
// number is what survives — every one of them was driven from complete
// crossings and every snapshot taken after one is valid.
func (s *StreamReader) finishStream() error {
	pending := s.sc.pending()
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
	if !s.sc.done {
		return &TruncationError{Kind: TruncatedUnterminated, Groups: s.next}
	}
	return nil
}

func (s *StreamReader) fail(err error) error {
	s.failed = err
	return err
}

// ---------------------------------------------------------------------------
// Following a growing file
// ---------------------------------------------------------------------------

// FollowPoll is how often `FollowFile` re-checks a file that has stopped
// growing. It is short enough that a snapshot lands promptly after the call
// that earns it and long enough not to spin a core.
const FollowPoll = 5 * time.Millisecond

// FollowFile returns a reader over a file the producer is still appending to.
//
// It is the adapter that makes "the daemon writes `trace.json` incrementally"
// fit `NewStreamReader`'s contract. At end of file it waits rather than
// returning `io.EOF`; it returns `io.EOF` only once `done` has been closed AND
// the file has been drained, so a recording still in progress is never mistaken
// for a truncated one.
//
// `done` is the caller's statement that the producer has stopped. Closing it
// early truncates the recording — which is reported as a `*TruncationError`
// rather than silently accepted, but is still the caller's error to avoid.
func FollowFile(path string, done <-chan struct{}) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening the boundary stream %s: %w", path, err)
	}
	return &followReader{f: f, done: done, poll: FollowPoll}, nil
}

type followReader struct {
	f    *os.File
	done <-chan struct{}
	poll time.Duration
	// drained records that the producer has stopped and the file has been read
	// to its end, so every later read is EOF.
	drained bool
}

func (r *followReader) Read(p []byte) (int, error) {
	if r.drained {
		return 0, io.EOF
	}
	for {
		n, err := r.f.Read(p)
		if n > 0 {
			return n, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		select {
		case <-r.done:
			// The producer has stopped, but it may have written between the
			// read above and this check. Drain what is left before declaring
			// the stream over.
			n, err = r.f.Read(p)
			if n > 0 {
				return n, nil
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return 0, err
			}
			r.drained = true
			return 0, io.EOF
		default:
		}
		time.Sleep(r.poll)
	}
}

func (r *followReader) Close() error { return r.f.Close() }

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
