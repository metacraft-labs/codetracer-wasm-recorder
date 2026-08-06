package wasmsnapshot

// Slice production — `WASM-Replay-Snapshots-And-Slices.md` §7.
//
// A recording is split into separate `.ct` containers at quiescent points, and
// each container is sealed and emitted *while the recording is still running*,
// mirroring `MCR-Client-Side-Splitting.md`:
//
//	page loads                → slice 0 opens
//	quiescent point 0 (base)  → snapshot → slice 0 sealed → emit slice 0
//	quiescent points 1..9     → incremental snapshots
//	quiescent point 10 (base) → snapshot → slice 1 sealed → emit slice 1
//	page unloads              → seal final slice + manifest
//
// # Why a slice is independently materialisable
//
// Every slice opens with a **base snapshot**: a complete resume point, because
// at a quiescent point linear memory + globals + tables are the module's entire
// state (§3). A consumer holding slice N — and nothing else — can restore that
// snapshot into a fresh instantiation and replay slice N's own range of
// exported calls, without touching slices 0..N-1. That is the property MCR
// relies on for parallel per-slice materialisation, and this package earns it
// in two places:
//
//   - each slice gets a **fresh page-CAS per-trace tier**, so its `snappages.ns`
//     holds every page its snapshots reference. Sharing one tier across slices
//     would make slice N's regions `kind=2` references to pages that only
//     slice 0's container carries — a set of containers that is only readable
//     in order, which is exactly what slicing exists to avoid.
//   - each slice's `snapshot.idx` lists only that slice's quiescent points, the
//     first flagged as its base, so the slice is self-describing (`Set.Range`).
//
// # INTERVAL vs SLICE
//
// `MCR-Omniscient-DB-Algorithms.md` § "INTERVAL vs SLICE (distinct units)"
// keeps the two apart and so does this file:
//
//   - A **slice** is the user-chosen distribution/storage unit — how the
//     recording is cut into separate containers. `SlicePolicy` is that choice,
//     and "no slicing at all" (a nil policy) stays the default.
//   - An **interval** is the engine-chosen lazy-population unit, sized for
//     first-seek latency and independent of slice size.
//
// This implementation does **not** model intervals as a distinct on-disk unit.
// What it has is snapshot *density* within a slice (`SliceOptions.SnapshotEvery`),
// which is the same idea — how finely a slice's range can be entered without
// re-executing from its base — configured by a knob that is deliberately
// separate from the two slice-size knobs. Calling it an interval would be
// overstating it: there is no interval record, no interval identity, and no
// lazy population; a snapshot-bearing point is simply a cheaper entry point.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/tracewriter"
)

// SlicePolicy is the user's choice of slice granularity (snapshot spec §7:
// "the user-chosen distribution/storage unit … Typically 1–50 MB, and possibly
// absent entirely").
//
// The zero value is meaningful: it seals nothing early, so the whole recording
// becomes one slice. That is MCR's `--no-split`. "Possibly absent entirely" —
// no slicing at all — is spelled by passing no policy to the replay, which is
// the default everywhere.
type SlicePolicy struct {
	// EveryPoints is the quiescent-point interval between slice bases: a slice
	// is sealed once the replay has advanced this many quiescent points past
	// the slice's base, which is also the number of exported calls the slice
	// covers. Zero disables the count trigger.
	//
	// It is the granularity snapshot spec §7's timeline is written in — "slice
	// 0 opens at point 0 … quiescent point 10 (base) → slice 1 sealed" is
	// EveryPoints = 10.
	EveryPoints int

	// TargetBytes seals a slice once its accumulated **snapshot payload**
	// reaches this many bytes. Zero disables the size trigger.
	//
	// The measured quantity is deliberately named: it is the region layout,
	// the inline page bytes, the globals, the tables and the per-trace page
	// store — everything this package will write into the slice's `snap*`
	// namespaces. It is **not** the sealed container's size, because the other
	// half of a slice is the materialised trace, which the CTFS writer buffers
	// opaquely and only sizes at `ProduceTrace`. Reporting a target this code
	// cannot measure would be worse than naming the one it can: the snapshot
	// payload is the term that grows with the module's memory, and it is the
	// term that decides whether a slice is 1 MB or 50.
	//
	// `SliceInfo.Bytes` reports each sealed slice's true on-disk size, so the
	// relationship between the two is observable rather than assumed.
	TargetBytes int64
}

// sealsAfter reports whether a slice that has advanced `span` quiescent points
// past its base and holds `payload` bytes of snapshot data has reached the
// user's granularity.
func (p SlicePolicy) sealsAfter(span int, payload int64) bool {
	if p.EveryPoints > 0 && span >= p.EveryPoints {
		return true
	}
	if p.TargetBytes > 0 && payload >= p.TargetBytes {
		return true
	}
	return false
}

func (p SlicePolicy) validate() error {
	if p.EveryPoints < 0 {
		return fmt.Errorf("wasmsnapshot: --slice-every cannot be negative (got %d)", p.EveryPoints)
	}
	if p.TargetBytes < 0 {
		return fmt.Errorf("wasmsnapshot: --slice-bytes cannot be negative (got %d)", p.TargetBytes)
	}
	return nil
}

// SliceInfo describes one sealed slice.
type SliceInfo struct {
	// Index is the slice's ordinal, 0-based.
	Index int `json:"index"`
	// File is the slice container's path relative to the manifest.
	File string `json:"file"`
	// BasePoint is the quiescent point the slice resumes from. Its snapshot is
	// flagged as a base in the slice's own `snapshot.idx`.
	BasePoint int `json:"base_point"`
	// EndPoint is the quiescent point the slice's range ends at. The slice
	// covers exported calls [BasePoint, EndPoint).
	EndPoint int `json:"end_point"`
	// ExportCalls is EndPoint - BasePoint, spelled out so a manifest reader
	// does not have to know that relationship.
	ExportCalls int `json:"export_calls"`
	// Snapshots counts the snapshot-bearing points in the slice, including the
	// base. Points without a snapshot are still listed in the slice's index.
	Snapshots int `json:"snapshots"`
	// Points counts every quiescent point the slice's index lists.
	Points int `json:"points"`
	// Bytes is the sealed container's size on disk.
	Bytes int64 `json:"bytes"`
}

// SliceManifest lists every slice a recording was split into.
//
// It is the counterpart of MCR's `manifest.amnf`
// (`MCR-Client-Side-Splitting.md`, "File organization in S3"): a small
// description of the set, written beside the slice containers rather than
// inside one of them, because it describes all of them and belongs to none.
// It is not snapshot data, so it does not conflict with §6's "snapshots are
// not sidecar files" — no slice's snapshots live outside its own `.ct`.
type SliceManifest struct {
	// Version is this manifest's schema revision.
	Version int `json:"version"`
	// Program is the recorded program's name.
	Program string `json:"program"`
	// TotalPoints is the number of quiescent points the whole recording had,
	// so a consumer can tell a complete slice set from a truncated one.
	TotalPoints int `json:"total_points"`
	// Slices are the sealed slices, in order. Their ranges tile [0,
	// TotalPoints-1] exactly: slice i's EndPoint is slice i+1's BasePoint.
	Slices []SliceInfo `json:"slices"`
}

// SliceManifestName is the manifest's filename inside the slice directory.
const SliceManifestName = "slices.manifest.json"

// SliceManifestVersion is the schema revision `SliceManifest` is written at.
const SliceManifestVersion = 1

// SliceOptions configures slice production.
type SliceOptions struct {
	// Dir is the directory the slice containers and the manifest are written
	// to. Required.
	Dir string
	// Program is the recorded program's name, e.g. "balance_calc.wasm". Slice
	// containers are named after its stem.
	Program string
	// Workdir is the workdir recorded into each slice's `meta.dat`.
	Workdir string
	// Policy is the user's slice granularity.
	Policy SlicePolicy
	// SnapshotEvery snapshots one quiescent point in N *within* a slice, over
	// and above the base snapshot every slice always carries. 0 or 1 means
	// every point. See the INTERVAL vs SLICE note in this file's header for
	// why this is not called an interval.
	SnapshotEvery int
	// UseSystemCache consults the host-wide page CAS tier when encoding.
	UseSystemCache bool
	// Encode configures the page-CAS encoding of each snapshot.
	Encode EncodeOptions
	// NewRecorder produces the trace recorder for a slice. Required — each
	// slice records into its own writer, which is what makes the slice's trace
	// cover its range and nothing else.
	NewRecorder func() tracewriter.TraceRecorder
	// OnSealed, when non-nil, is called as soon as a slice container exists on
	// disk — before the recording continues. This is the seam an uploader
	// plugs into to overlap upload with recording
	// (`MCR-Client-Side-Splitting.md` M18b). No uploader is implemented here:
	// this repository produces slices, it does not talk to the CI monolith.
	OnSealed func(SliceInfo) error
}

func (o SliceOptions) validate() error {
	if o.Dir == "" {
		return fmt.Errorf("wasmsnapshot: slice production needs an output directory")
	}
	if o.NewRecorder == nil {
		return fmt.Errorf(
			"wasmsnapshot: slice production needs a NewRecorder; a slice whose trace " +
				"was never recorded would be a base snapshot with no execution in it")
	}
	if o.SnapshotEvery < 0 {
		return fmt.Errorf("wasmsnapshot: SnapshotEvery cannot be negative (got %d)", o.SnapshotEvery)
	}
	return o.Policy.validate()
}

// SliceWriter turns a replay's quiescent points into sealed slice containers.
//
// It is driven entirely from `boundarylog.Options.AtQuiescentPoint`, which is
// the only place snapshot spec §3 permits state to be captured, and is also
// the only place a trace recorder may be swapped: the recorder's call-depth
// counter has to start from an empty WASM stack.
type SliceWriter struct {
	opts SliceOptions

	// open is the slice being filled, or nil before the first point.
	open *openSlice
	// sealed lists the slices already written out.
	sealed []SliceInfo
	// lastPoint is the highest quiescent point seen, so Finish knows where the
	// open slice ends.
	lastPoint int
	// mod is the live module, kept so a seal can detach the recorder it is
	// about to drain even when the seal is driven from Finish rather than from
	// a quiescent-point hook.
	mod api.Module
	// finished guards against a second Finish.
	finished bool
}

// openSlice is the mutable state of the slice currently being filled.
type openSlice struct {
	index     int
	basePoint int
	// points counts the quiescent points added to this slice's index,
	// including the base.
	points int
	// builder holds this slice's own snapshot streams and its own per-trace
	// page tier.
	builder *Builder
	// recorder is the trace recorder attached to the module for this slice's
	// range.
	recorder tracewriter.TraceRecorder
}

// NewSliceWriter starts slice production.
func NewSliceWriter(opts SliceOptions) (*SliceWriter, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("wasmsnapshot: creating the slice directory: %w", err)
	}
	return &SliceWriter{opts: opts}, nil
}

// AtQuiescentPoint is the replay hook. It must be called at every quiescent
// point the replay passes through, in order, starting at the point the
// recording begins from.
func (w *SliceWriter) AtQuiescentPoint(point int, mod api.Module) error {
	if w.finished {
		return fmt.Errorf(
			"wasmsnapshot: quiescent point %d arrived after the slice set was finished",
			point)
	}
	w.mod = mod
	if w.open == nil {
		if err := w.openSliceAt(point, mod); err != nil {
			return err
		}
		w.lastPoint = point
		return nil
	}
	if point <= w.lastPoint {
		return fmt.Errorf(
			"wasmsnapshot: quiescent point %d arrived after point %d; slices are sealed "+
				"in order and cannot be rewound", point, w.lastPoint)
	}
	w.lastPoint = point

	// Record the point in the open slice's index, snapshotting it if the
	// density calls for it.
	if err := w.addPoint(point, mod, w.snapshotDue(point)); err != nil {
		return err
	}

	// A slice may only be sealed where the next one can open — at a quiescent
	// point — so the decision is taken here and the seal happens immediately,
	// with this same point becoming the next slice's base.
	if w.opts.Policy.sealsAfter(point-w.open.basePoint, w.open.builder.PayloadBytes()) {
		if err := w.seal(point); err != nil {
			return err
		}
		return w.openSliceAt(point, mod)
	}
	return nil
}

// snapshotDue reports whether the density knob wants a snapshot at `point`.
func (w *SliceWriter) snapshotDue(point int) bool {
	every := w.opts.SnapshotEvery
	if every <= 1 {
		return true
	}
	// Counted from the slice's base so density is a property of the slice
	// rather than of where the slice happens to start.
	return (point-w.open.basePoint)%every == 0
}

// openSliceAt starts a new slice whose base is `point`, capturing the base
// snapshot and attaching a fresh recorder to the module.
func (w *SliceWriter) openSliceAt(point int, mod api.Module) error {
	builder, err := NewIncrementalBuilder(NewTiers(w.opts.UseSystemCache), w.opts.Encode)
	if err != nil {
		return err
	}
	w.open = &openSlice{index: len(w.sealed), basePoint: point, builder: builder}

	// The base snapshot is mandatory: it is the whole reason the slice is
	// independently materialisable, so it is not subject to the density knob.
	if err := w.addPoint(point, mod, true); err != nil {
		return err
	}
	if err := builder.MarkBase(point); err != nil {
		return err
	}

	w.open.recorder = w.opts.NewRecorder()
	if err := attachRecorder(mod, w.open.recorder); err != nil {
		return err
	}
	return nil
}

// addPoint lists `point` in the open slice's index, with or without a
// snapshot.
func (w *SliceWriter) addPoint(point int, mod api.Module, snapshot bool) error {
	// A slice's index describes the slice, so CrossingSeq is not carried here:
	// it is a property of the whole recording's boundary log, which a slice
	// consumer reads separately. -1 is the encoding for "no next crossing".
	if err := w.open.builder.AddPoint(QuiescentPoint{
		Ordinal:       point,
		ExportsBefore: point,
		CrossingSeq:   -1,
	}); err != nil {
		return err
	}
	w.open.points++
	if !snapshot {
		return nil
	}
	snap, err := Capture(mod, point)
	if err != nil {
		return err
	}
	return w.open.builder.Add(snap)
}

// seal writes the open slice out as a standalone `.ct` and reports it.
func (w *SliceWriter) seal(endPoint int) error {
	s := w.open
	if s == nil {
		return fmt.Errorf("wasmsnapshot: no slice is open")
	}
	w.open = nil

	// Detach the recorder before draining it. Nothing executes between a seal
	// and the next slice's open, so this is belt-and-braces — but a recorder
	// the interpreter still holds while `ProduceTrace` walks its buffer is the
	// kind of aliasing that only shows up under a change made much later.
	if err := attachRecorder(w.mod, nil); err != nil {
		return err
	}

	name := SliceProgramName(w.opts.Program, s.index)
	if err := s.recorder.ProduceTrace(w.opts.Dir, name, w.opts.Workdir); err != nil {
		return fmt.Errorf("wasmsnapshot: producing slice %d's trace: %w", s.index, err)
	}
	file := sliceContainerName(w.opts.Program, s.index)
	path := filepath.Join(w.opts.Dir, file)
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf(
			"wasmsnapshot: the trace writer was expected to produce slice %d at %s: %w",
			s.index, path, err)
	}

	// The snapshot namespaces go INSIDE the slice container (§6). This is what
	// makes the file a self-sufficient unit: trace bytes plus the base
	// snapshot plus every page that snapshot references.
	if err := s.builder.AttachTo(path); err != nil {
		return fmt.Errorf("wasmsnapshot: attaching slice %d's snapshots: %w", s.index, err)
	}
	if st, err = os.Stat(path); err != nil {
		return err
	}

	info := SliceInfo{
		Index:       s.index,
		File:        file,
		BasePoint:   s.basePoint,
		EndPoint:    endPoint,
		ExportCalls: endPoint - s.basePoint,
		Snapshots:   s.builder.SnapshotCount(),
		Points:      s.builder.PointCount(),
		Bytes:       st.Size(),
	}
	w.sealed = append(w.sealed, info)

	if w.opts.OnSealed != nil {
		if err := w.opts.OnSealed(info); err != nil {
			return fmt.Errorf("wasmsnapshot: handing off slice %d: %w", s.index, err)
		}
	}
	return nil
}

// Finish seals the slice still open and writes the manifest. It returns the
// manifest and the path it was written to.
func (w *SliceWriter) Finish() (*SliceManifest, string, error) {
	if w.finished {
		return nil, "", fmt.Errorf("wasmsnapshot: the slice set is already finished")
	}
	w.finished = true
	if w.open != nil {
		if w.open.basePoint == w.lastPoint {
			// The replay stopped at the point the slice opened at, so the
			// slice covers no exported call. Sealing it would emit a container
			// holding a base snapshot and an empty trace, which a consumer
			// cannot tell from a slice whose range failed to record. Discard it
			// instead — its base point is already the previous slice's end.
			if err := attachRecorder(w.mod, nil); err != nil {
				return nil, "", err
			}
			w.open = nil
		} else if err := w.seal(w.lastPoint); err != nil {
			return nil, "", err
		}
	}
	if len(w.sealed) == 0 {
		return nil, "", fmt.Errorf(
			"wasmsnapshot: no slice was produced; the replay passed through no quiescent " +
				"point that closed an exported call")
	}

	m := &SliceManifest{
		Version:     SliceManifestVersion,
		Program:     w.opts.Program,
		TotalPoints: w.lastPoint + 1,
		Slices:      w.sealed,
	}
	if err := checkTiling(m.Slices); err != nil {
		return nil, "", err
	}

	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(w.opts.Dir, SliceManifestName)
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return nil, "", fmt.Errorf("wasmsnapshot: writing the slice manifest: %w", err)
	}
	return m, path, nil
}

// checkTiling verifies that a slice set partitions the recording: slice i ends
// exactly where slice i+1 begins, with no gap and no overlap.
//
// That invariant is what lets a consumer materialise the whole recording from
// the slice set — a gap would lose a range of exported calls silently, and an
// overlap would materialise one twice — so it is checked before the manifest
// that advertises the set is written.
//
// `SliceWriter` cannot currently produce a violation: `AtQuiescentPoint` opens
// each new slice at the very point the previous one was sealed at, and refuses
// a point that does not advance. This is therefore an assertion about that
// logic rather than a validation of untrusted input, and it is a separate
// function so it can be tested directly — a defensive check that no test can
// reach is indistinguishable from one that does not work.
func checkTiling(slices []SliceInfo) error {
	for i, s := range slices {
		if i == 0 {
			continue
		}
		if s.BasePoint != slices[i-1].EndPoint {
			return fmt.Errorf(
				"wasmsnapshot: slice %d starts at quiescent point %d but slice %d ended at "+
					"%d; the slice set does not tile the recording",
				s.Index, s.BasePoint, i-1, slices[i-1].EndPoint)
		}
	}
	return nil
}

// Slices reports the slices sealed so far. It is how a caller observes that
// slices really are emitted *during* the replay rather than at the end.
func (w *SliceWriter) Slices() []SliceInfo {
	return append([]SliceInfo(nil), w.sealed...)
}

// LoadSliceManifest reads a slice manifest written by `Finish`.
func LoadSliceManifest(path string) (*SliceManifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m SliceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", path, err)
	}
	if m.Version != SliceManifestVersion {
		return nil, fmt.Errorf(
			"%s is manifest version %d; this build writes and reads version %d",
			path, m.Version, SliceManifestVersion)
	}
	return &m, nil
}

// ---------------------------------------------------------------------------
// Naming
// ---------------------------------------------------------------------------

// SliceProgramName is the program name a slice's trace is produced under.
//
// `MCR-Client-Side-Splitting.md` names slices `0000.ct`, but the CTFS writer
// derives a container's filename from the program name it is given and writes
// that same name into `meta.dat`, so bare `0000` would erase the recorded
// program's identity from every slice. Interpolating the index into the stem
// keeps both: a distinct filename per slice and a `meta.dat` that still says
// what was recorded.
func SliceProgramName(program string, index int) string {
	base := filepath.Base(program)
	ext := filepath.Ext(base)
	return fmt.Sprintf("%s-%04d%s", strings.TrimSuffix(base, ext), index, ext)
}

// sliceContainerName is the `.ct` filename `SliceProgramName` results in.
func sliceContainerName(program string, index int) string {
	name := SliceProgramName(program, index)
	return strings.TrimSuffix(name, filepath.Ext(name)) + ".ct"
}

// ---------------------------------------------------------------------------
// Recorder attachment
// ---------------------------------------------------------------------------

// attachRecorder points a live module's interpreter at `rec`.
//
// `ModuleInstance.Record` is read live by the interpreter loop, so swapping it
// is well defined only at a quiescent point: the recorder's own call-depth
// bookkeeping has to start from an empty WASM stack. Every caller here is a
// quiescent-point hook.
//
// `SetRecorder` rather than a bare field write, because the module also holds
// the "which DWARF types has the recorder already been told about" memo, and a
// new slice's recorder has been told about none of them. See that method.
func attachRecorder(mod api.Module, rec tracewriter.TraceRecorder) error {
	mi, err := moduleInstance(mod)
	if err != nil {
		return err
	}
	mi.SetRecorder(rec)
	return nil
}
