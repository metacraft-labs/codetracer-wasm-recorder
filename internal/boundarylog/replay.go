package boundarylog

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/tracewriter"
)

// hookModuleName is the host module the instrumenter's hooks are imported
// under (`DEFAULT_HOST_MODULE` in `hooks.rs`). Replay must be handed the
// **original, uninstrumented** module (spec §6.1): the interpreter observes
// execution from the inside, so the rewrite is a recording-time device
// only. Seeing these imports means the caller passed the instrumented
// module, which would shift every import index the recording refers to.
const hookModuleName = "__codetracer"

// Options configures a replay.
type Options struct {
	// Runtime is the wazero runtime the guest and the synthesised host
	// modules are instantiated into. Required.
	Runtime wazero.Runtime
	// Compiled is the compiled **original** guest module. Required.
	Compiled wazero.CompiledModule
	// Recording is the parsed boundary recording. Required.
	Recording *Recording
	// Manifest is the optional `ct-instrument` sidecar. When present its
	// boundary signatures are cross-checked against the module's own type
	// section and a disagreement aborts the replay.
	Manifest *Manifest
	// ModuleConfig configures the guest instantiation. The caller should
	// have already disabled start functions with `WithStartFunctions()`:
	// replay drives the recorded exported calls itself and must not also
	// run `_start`.
	ModuleConfig wazero.ModuleConfig
	// Recorder receives the materialised trace events. May be nil, in
	// which case the replay still runs and still checks for divergence
	// but produces no trace.
	Recorder tracewriter.TraceRecorder

	// --- quiescent points and range replay -----------------------------
	//
	// These implement the seeking half of the boundary model, specified in
	// `WASM-Replay-Snapshots-And-Slices.md`. A *quiescent point* is a moment
	// when no exported function is executing: point 0 is the freshly
	// instantiated module, point k is after the k-th top-level exported call
	// has returned. There are `len(TopLevelExports()) + 1` of them, and the
	// WASM stack is empty at every one — which is what makes them the only
	// places a snapshot can be taken or resumed from (that spec's §3).
	//
	// `internal/wasmsnapshot` is the consumer of all three fields; they live
	// here rather than there because the driver is what knows where the
	// points are, and because a snapshot taken anywhere else would be silently
	// wrong rather than loudly refused.

	// AtQuiescentPoint, when non-nil, is called at every quiescent point the
	// replay passes through: at FromPoint before the first driven call, and
	// after each call returns. An error from it aborts the replay.
	AtQuiescentPoint func(point int, guest api.Module) error

	// FromPoint is the quiescent point the replay starts at. 0 (the default)
	// replays from the beginning.
	FromPoint int

	// ToPoint is the quiescent point the replay stops at. 0 (the default)
	// means "to the end of the recording".
	ToPoint int

	// Resume must restore the module's state to the captured state at
	// FromPoint. It is called once, after instantiation and before the first
	// driven call.
	//
	// It is **required** whenever FromPoint > 0. Resuming into a freshly
	// instantiated module would re-execute a suffix against a prefix's worth
	// of missing state — producing a trace that describes an execution that
	// never happened while being indistinguishable on disk from a faithful
	// one. That is the same failure spec §6's divergence rule exists to
	// prevent, so it is refused rather than defaulted.
	Resume func(guest api.Module) error
}

// Result reports what a successful replay did.
type Result struct {
	// ExportCalls is the number of exported functions replay invoked.
	ExportCalls int
	// ImportCalls is the number of imported calls replay serviced from
	// the recording.
	ImportCalls int
	// UncheckedImportCalls counts calls to imports whose signature is
	// `() -> ()` **in a recording older than M39**, where nothing on disk
	// can be matched to them: such a crossing contributes no value runs and
	// no Call/Return record, and the realm markers that do name its index
	// were spelled the same as an export's, so `LoadRecording` recovers no
	// crossing for it.
	//
	// It is always 0 for a recording whose markers name the import edge
	// (`Recording.MarkersIdentifyImports`). There the crossing is recovered
	// like any other and a call that does not match it is a spec §6
	// divergence — the fallback is not available, because "replayed
	// unchecked" is exactly the silent degradation §8 exists to prevent.
	// It survives only so an already-recorded trace replays rather than
	// being rejected for its age.
	UncheckedImportCalls int
	// FromPoint and ToPoint are the quiescent-point range that was actually
	// driven, after Options.FromPoint / Options.ToPoint were resolved
	// against the recording's length.
	FromPoint int
	ToPoint   int
}

// DivergenceError is the spec §6 hard failure: replay reached a point where
// the module's behaviour differs from the recording. It is never downgraded
// to a warning, and the caller must not write a trace after seeing one — a
// divergence means the recording is missing an input, so the trace would be
// a fabrication.
type DivergenceError struct {
	// What names the kind of divergence ("import argument", "exported
	// return value", …).
	What string
	// Where names the crossing.
	Where string
	// Recorded and Actual are the two sides of the mismatch, already
	// rendered.
	Recorded string
	Actual   string
	// Detail is optional extra context.
	Detail string
}

func (e *DivergenceError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "boundary replay diverged from the recording: %s mismatch at %s\n",
		e.What, e.Where)
	fmt.Fprintf(&b, "  recorded: %s\n", e.Recorded)
	fmt.Fprintf(&b, "  actual:   %s", e.Actual)
	if e.Detail != "" {
		fmt.Fprintf(&b, "\n  %s", e.Detail)
	}
	b.WriteString("\n\nA divergence means the recording is missing an input " +
		"(WASM-Instrumentation-Layer.md §6). No trace has been written.")
	return b.String()
}

// importPlan is everything replay needs about one imported function.
type importPlan struct {
	index      uint32
	module     string
	name       string
	sig        Signature
	paramTypes []api.ValueType
	resultTyps []api.ValueType
}

func (p *importPlan) describe() string {
	return fmt.Sprintf("import #%d (%s.%s)%s", p.index, p.module, p.name, p.sig)
}

// replayer holds the mutable state of one replay.
type replayer struct {
	opts    Options
	imports []importPlan
	// planned guards `planImports` against running twice.
	planned bool
	// cursor is the index of the next crossing the recording expects.
	// Every export call and every checked import call advances it, so the
	// exact *global interleaving* of the recording is verified, not just
	// each import's own call count.
	cursor int
	// err holds the first divergence seen. Once set, later stub
	// invocations short-circuit: the module keeps running until wazero
	// unwinds, and the driver reports this error rather than whatever
	// secondary failure followed.
	err    error
	result Result
	// guest is set once the module is instantiated, so mutation
	// application can reach its imported memories.
	providers map[string]api.Module
}

// Replay re-executes the recorded module against its boundary recording and
// returns once every recorded exported call has been driven.
//
// It implements spec §6 step by step:
//
//  1. instantiate the original, uninstrumented module;
//  2. supply every import with a stub that, on the n-th call, asserts the
//     recorded arguments match and returns the recorded results;
//  3. apply §3.3 initial state before the first export call and §3.4
//     mutations at their recorded points;
//  4. invoke each recorded export call with its recorded arguments;
//  5. leave trace emission to the interpreter, which is already recording.
//
// A divergence returns a *DivergenceError and the caller MUST NOT write a
// trace.
func Replay(ctx context.Context, opts Options) (Result, error) {
	r := &replayer{opts: opts, providers: map[string]api.Module{}}
	return r.run(ctx)
}

// prepare performs every step of spec §6 that precedes the first driven call:
// resolving the imports, satisfying them, instantiating the guest and applying
// §3.3 initial state.
//
// It is shared by the batch driver (`run`) and the streaming driver
// (`StreamingReplay`), because none of it depends on the recording's crossings
// — only on the module and on the host state — and so none of it has to wait
// for a stream to finish arriving.
func (r *replayer) prepare(ctx context.Context) (api.Module, error) {
	if err := r.planImports(); err != nil {
		return nil, err
	}
	if err := r.checkImportedMemories(); err != nil {
		return nil, err
	}

	// --- spec §6.2 and §3.3: satisfy every one of the guest's imports ----
	//
	// Both the import stubs and any host-supplied memory/global state must
	// exist before the guest is instantiated, because the guest imports
	// from them.
	if err := r.instantiateHostModules(ctx); err != nil {
		return nil, err
	}

	cfg := r.opts.ModuleConfig
	if cfg == nil {
		cfg = wazero.NewModuleConfig()
	}
	guest, err := r.opts.Runtime.InstantiateModuleWithRecord(
		ctx, r.opts.Compiled, cfg, r.opts.Recorder)
	if err != nil {
		return nil, fmt.Errorf("instantiating the recorded module for replay: %w", err)
	}

	// The initial *contents* of an imported memory are written after
	// instantiation: the provider module declares the memory's shape, and
	// this fills it in. Both happen before the first exported call, as
	// spec §3.3 requires.
	if err := r.applyInitialMemory(); err != nil {
		return nil, err
	}
	return guest, nil
}

// refuseNestedExports is spec §8's discipline: a recording containing an
// exported call made while another crossing was open cannot be replayed, so it
// is refused rather than silently replayed wrong.
func refuseNestedExports(nested []*Crossing) error {
	if len(nested) == 0 {
		return nil
	}
	names := make([]string, 0, len(nested))
	for _, c := range nested {
		names = append(names, c.Describe())
	}
	return fmt.Errorf(
		"boundary recording contains %d exported call(s) made while another "+
			"crossing was open (%s). Replaying a host callback would need the "+
			"host's own control flow, which a boundary log does not carry; such "+
			"a recording is refused rather than silently replayed wrong (spec §8)",
		len(nested), strings.Join(names, ", "))
}

func (r *replayer) run(ctx context.Context) (Result, error) {
	rec := r.opts.Recording
	if rec == nil {
		return r.result, fmt.Errorf("boundary replay: no recording supplied")
	}

	// Import planning first, so handing replay the *instrumented* module is
	// reported as the mistake it is rather than as a mismatched export.
	if err := r.planImports(); err != nil {
		return r.result, err
	}
	if err := r.checkExports(); err != nil {
		return r.result, err
	}
	if err := refuseNestedExports(rec.NestedExports()); err != nil {
		return r.result, err
	}

	guest, err := r.prepare(ctx)
	if err != nil {
		return r.result, err
	}

	// --- spec §6.4: invoke each recorded export call ---------------------
	exports := rec.TopLevelExports()
	from, to, err := r.resolveRange(len(exports))
	if err != nil {
		return r.result, err
	}

	if from > 0 {
		// Seed the crossing cursor at the crossing the resumed point
		// precedes, so import crossings keep lining up with the recording
		// after the seek. Every crossing before it belongs to the skipped
		// prefix.
		//
		// The final quiescent point is a legal seek target — it is the point
		// after the last exported call returned — but it precedes no crossing
		// at all, so there is no `exports[from]` to read. The recording is
		// exhausted there, which is exactly what a cursor at the end means.
		if from < len(exports) {
			r.cursor = exports[from].Seq
		} else {
			r.cursor = len(rec.Crossings)
		}
		if err := r.opts.Resume(guest); err != nil {
			return r.result, fmt.Errorf(
				"restoring the module state at quiescent point %d: %w", from, err)
		}
	}
	if err := r.atQuiescentPoint(from, guest); err != nil {
		return r.result, err
	}
	for i := from; i < to; i++ {
		if err := r.callExport(ctx, guest, exports[i]); err != nil {
			return r.result, err
		}
		if err := r.atQuiescentPoint(i+1, guest); err != nil {
			return r.result, err
		}
	}
	r.result.FromPoint, r.result.ToPoint = from, to

	if to != len(exports) || from != 0 {
		// A partial range legitimately leaves crossings unconsumed at both
		// ends, so the whole-recording check below does not apply. The
		// per-crossing checks inside the range have all run.
		return r.result, nil
	}

	// Every crossing the recording carries must have been consumed. A
	// leftover means the module stopped calling out earlier than it did
	// when recorded — the mirror image of an unrecorded call, and just as
	// much a divergence.
	if r.cursor != len(rec.Crossings) {
		next := &rec.Crossings[r.cursor]
		return r.result, &DivergenceError{
			What:     "crossing count",
			Where:    fmt.Sprintf("end of replay (%d of %d crossings consumed)", r.cursor, len(rec.Crossings)),
			Recorded: next.Describe(),
			Actual:   "replay finished without reaching it",
			Detail: "the module stopped crossing the boundary earlier than it did " +
				"when recorded",
		}
	}

	return r.result, nil
}

// resolveRange validates Options.FromPoint / Options.ToPoint against the
// recording and returns the half-open range of top-level exports to drive.
//
// `nExports` top-level calls give `nExports + 1` quiescent points, numbered
// 0..nExports; driving the range [from, to) of calls moves the module from
// quiescent point `from` to quiescent point `to`.
func (r *replayer) resolveRange(nExports int) (from, to int, err error) {
	from, to = r.opts.FromPoint, r.opts.ToPoint
	if to == 0 {
		to = nExports
	}
	if from < 0 || from > nExports {
		return 0, 0, fmt.Errorf(
			"boundary replay: quiescent point %d does not exist; the recording has "+
				"%d top-level exported call(s), so its points are 0..%d",
			from, nExports, nExports)
	}
	if to < from || to > nExports {
		return 0, 0, fmt.Errorf(
			"boundary replay: quiescent-point range [%d,%d] is not within 0..%d",
			from, to, nExports)
	}
	if from > 0 && r.opts.Resume == nil {
		return 0, 0, fmt.Errorf(
			"boundary replay: resuming at quiescent point %d needs an Options.Resume "+
				"that restores the module's state there. Replaying a suffix against a "+
				"freshly instantiated module would materialise a trace of an execution "+
				"that never happened, so it is refused rather than defaulted "+
				"(WASM-Replay-Snapshots-And-Slices.md §3)", from)
	}
	return from, to, nil
}

// atQuiescentPoint invokes the caller's hook, if any.
func (r *replayer) atQuiescentPoint(point int, guest api.Module) error {
	if r.opts.AtQuiescentPoint == nil {
		return nil
	}
	if err := r.opts.AtQuiescentPoint(point, guest); err != nil {
		return fmt.Errorf("at quiescent point %d: %w", point, err)
	}
	return nil
}

// planImports resolves every imported function's signature, taking it from
// the module's own type section and cross-checking it against the manifest
// when one is supplied.
//
// It is idempotent: both `run` (which calls it early, so that being handed the
// instrumented module is reported before anything else) and `prepare` (which
// needs it for the streaming path, where there is no early call) invoke it.
func (r *replayer) planImports() error {
	if r.planned {
		return nil
	}
	r.planned = true
	defs := r.opts.Compiled.ImportedFunctions()
	// `ImportedFunctions` returns definitions in function-import index
	// order, which is exactly the order `ct-instrument` numbers imports
	// in (it counts every function import, skipping only its own hooks —
	// and the original module carries none of those).
	for i, def := range defs {
		module, name, _ := def.Import()
		if module == hookModuleName {
			return fmt.Errorf(
				"the module imports %s.%s: this is the INSTRUMENTED module, but "+
					"replay requires the ORIGINAL, uninstrumented one (spec §6.1). "+
					"The instrumentation is a recording-time device; its extra "+
					"imports would shift every import index the recording refers to",
				module, name)
		}
		plan := importPlan{
			index:      uint32(i),
			module:     module,
			name:       name,
			paramTypes: def.ParamTypes(),
			resultTyps: def.ResultTypes(),
		}
		for j, vt := range plan.paramTypes {
			t, err := ScalarTypeOf(vt)
			if err != nil {
				return fmt.Errorf("%s.%s param %d: %w", module, name, j, err)
			}
			plan.sig.Params = append(plan.sig.Params, t)
		}
		for j, vt := range plan.resultTyps {
			t, err := ScalarTypeOf(vt)
			if err != nil {
				return fmt.Errorf("%s.%s result %d: %w", module, name, j, err)
			}
			plan.sig.Results = append(plan.sig.Results, t)
		}
		if b := r.opts.Manifest.Boundary(FuncKindImport, uint32(i)); b != nil {
			msig, err := b.Signature()
			if err != nil {
				return err
			}
			if !msig.Equal(plan.sig) {
				return fmt.Errorf(
					"instrumentation manifest and module disagree about import #%d "+
						"(%s.%s): manifest says %s, module says %s. The recording was "+
						"made from a different build of this module",
					i, module, name, msig, plan.sig)
			}
			if b.Name != "" && b.Name != name {
				return fmt.Errorf(
					"instrumentation manifest and module disagree about import #%d: "+
						"manifest names it %q, module names it %q. The recording was "+
						"made from a different build of this module",
					i, b.Name, name)
			}
		}
		r.imports = append(r.imports, plan)
	}
	return nil
}

// checkExports verifies that every export the recording calls exists in the
// module with a compatible signature, before anything is instantiated, so a
// mismatched module fails with a clear message rather than mid-replay.
func (r *replayer) checkExports() error {
	for _, c := range r.opts.Recording.TopLevelExports() {
		if err := r.checkExportCrossing(c); err != nil {
			return err
		}
	}
	return nil
}

// checkExportCrossing validates one recorded exported call against the module.
//
// The streaming driver calls this per crossing as it arrives, which is the only
// thing it *can* do: a stream has no "every export" to iterate before the first
// call is driven. The batch driver keeps running it over all of them up front,
// so a mismatched module still fails before anything is instantiated.
func (r *replayer) checkExportCrossing(c *Crossing) error {
	exported := r.opts.Compiled.ExportedFunctions()
	if c.Name == "" {
		return fmt.Errorf(
			"%s has no function name: the recording was made without a "+
				"`ct-instrument` manifest, so its exported calls cannot be "+
				"matched to the module's exports", c.Describe())
	}
	def, ok := exported[c.Name]
	if !ok {
		names := make([]string, 0, len(exported))
		for n := range exported {
			names = append(names, n)
		}
		sort.Strings(names)
		return fmt.Errorf(
			"%s: the module exports no function named %q (it exports: %s)",
			c.Describe(), c.Name, strings.Join(names, ", "))
	}
	sig, err := signatureOf(def)
	if err != nil {
		return fmt.Errorf("export %q: %w", c.Name, err)
	}
	if b := r.opts.Manifest.ExportBoundaryByName(c.Name); b != nil {
		msig, err := b.Signature()
		if err != nil {
			return err
		}
		if !msig.Equal(sig) {
			return fmt.Errorf(
				"instrumentation manifest and module disagree about export %q: "+
					"manifest says %s, module says %s. The recording was made "+
					"from a different build of this module", c.Name, msig, sig)
		}
	}
	if len(c.Args) != len(sig.Params) {
		return &DivergenceError{
			What:     "exported call arity",
			Where:    c.Describe(),
			Recorded: fmt.Sprintf("%d argument(s)", len(c.Args)),
			Actual:   fmt.Sprintf("the module's %q takes %d %s", c.Name, len(sig.Params), sig),
		}
	}
	if len(sig.Results) > 0 && !c.hasResults {
		return &DivergenceError{
			What:     "exported return value",
			Where:    c.Describe(),
			Recorded: "no result values",
			Actual:   fmt.Sprintf("the module's %q returns %d value(s) %s", c.Name, len(sig.Results), sig),
			Detail: "the recording carries no `:ret<n>` bindings for this call, " +
				"so the replayed return value cannot be checked against anything",
		}
	}
	return nil
}

func signatureOf(def api.FunctionDefinition) (Signature, error) {
	var sig Signature
	for i, vt := range def.ParamTypes() {
		t, err := ScalarTypeOf(vt)
		if err != nil {
			return sig, fmt.Errorf("param %d: %w", i, err)
		}
		sig.Params = append(sig.Params, t)
	}
	for i, vt := range def.ResultTypes() {
		t, err := ScalarTypeOf(vt)
		if err != nil {
			return sig, fmt.Errorf("result %d: %w", i, err)
		}
		sig.Results = append(sig.Results, t)
	}
	return sig, nil
}

// checkImportedMemories refuses a recording that does not carry the initial
// contents of a memory the module imports.
//
// Supplying a zeroed memory instead would be exactly the failure mode spec
// §8 exists to prevent: the replay would proceed and diverge later, at a
// point unrelated to the cause. A module that defines its own memory needs
// none of this — the `.wasm` already contains it — so this only fires when
// the memory is genuinely host-supplied (spec §3.3).
//
// Imported *globals* cannot be checked the same way: wazero's
// CompiledModule exposes ImportedMemories but has no ImportedGlobals
// counterpart. A missing imported global therefore surfaces as wazero's own
// instantiation error, which does name the missing export.
func (r *replayer) checkImportedMemories() error {
	state := r.opts.Recording.HostState
	for _, def := range r.opts.Compiled.ImportedMemories() {
		module, name, _ := def.Import()
		found := false
		if state != nil {
			for _, m := range state.Initial.Memories {
				if m.Module == module && m.Name == name {
					found = true
					break
				}
			}
		}
		if !found {
			return fmt.Errorf(
				"the module imports memory %s.%s, but the boundary recording carries "+
					"no initial contents for it. An imported memory's initial contents "+
					"are host-supplied state the recording must capture (spec §3.3); "+
					"replaying against a zeroed memory would diverge later, at a point "+
					"unrelated to the cause, so it is refused (spec §8). Expected a %q "+
					"sidecar in the recording declaring it",
				module, name, HostStateFileName)
		}
	}
	return nil
}

// stubModulePrefix namespaces the internal host modules the Go stubs are
// registered under when their name must also carry memory or global state.
// A guest never imports from these directly — only the synthesised provider
// module does, re-exporting them under the names the guest asked for.
const stubModulePrefix = "\x00ct-replay-stubs\x00"

// instantiateHostModules satisfies every one of the guest's imports before
// it is instantiated: the generated function stubs (spec §6.2) and the
// host-supplied memories and globals (spec §3.3).
//
// This is the generalisation of `internal/stylus`: instead of one
// hard-coded `vm_hooks` module whose Go stubs know the Stylus schema, the
// stubs here are generated from the boundary signatures and driven by the
// recording.
//
// A single host module name routinely supplies both kinds of entity —
// `env` with a memory and a handful of functions is the common case — but
// wazero's HostModuleBuilder can only export functions. Where both are
// needed, the Go stubs are registered under an internal name and a
// synthesised provider module imports and re-exports them alongside the
// memories and globals it defines. Where only functions are needed, the
// host module builder is used directly and no module is synthesised.
func (r *replayer) instantiateHostModules(ctx context.Context) error {
	names, specs := r.opts.Recording.HostState.providerModules()
	getSpec := func(name string) *providerSpec {
		s, ok := specs[name]
		if !ok {
			s = &providerSpec{module: name}
			specs[name] = s
			names = append(names, name)
		}
		return s
	}

	// Attach each imported function to the spec for its module name.
	for i := range r.imports {
		p := &r.imports[i]
		spec := getSpec(p.module)
		for _, existing := range spec.funcs {
			if existing.name == p.name {
				// The same (module, name) imported twice occupies two
				// import indices but is a single export. The recording
				// distinguishes them by index, which cannot be honoured
				// through one stub, so refuse rather than mis-attribute.
				return fmt.Errorf(
					"the module imports %s.%s more than once; replay cannot tell "+
						"the two import indices apart through a single host export",
					p.module, p.name)
			}
		}
		spec.funcs = append(spec.funcs, p)
	}
	sort.Strings(names)

	for _, name := range names {
		spec := specs[name]
		needsProvider := len(spec.memories) > 0 || len(spec.globals) > 0

		// Register the Go stubs: directly under the guest-visible name
		// when nothing else needs that name, otherwise under an internal
		// name the provider re-exports from.
		stubModule := name
		if needsProvider && len(spec.funcs) > 0 {
			stubModule = stubModulePrefix + name
			spec.stubModule = stubModule
		}
		if len(spec.funcs) > 0 {
			builder := r.opts.Runtime.NewHostModuleBuilder(stubModule)
			for _, p := range spec.funcs {
				plan := p
				builder = builder.NewFunctionBuilder().
					WithGoModuleFunction(
						api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
							r.serviceImport(plan, mod, stack)
						}),
						plan.paramTypes, plan.resultTyps).
					WithName(plan.name).
					Export(plan.name)
			}
			if _, err := builder.Instantiate(ctx); err != nil {
				return fmt.Errorf("instantiating replay stubs for host module %q: %w", name, err)
			}
		}

		if !needsProvider {
			continue
		}

		// Grow each declared minimum so it can hold every recorded region:
		// a recording whose data runs past the declared size would
		// otherwise fail opaquely on the first write.
		for i := range spec.memories {
			m := &spec.memories[i]
			for _, d := range m.Data {
				bytes, err := d.decode()
				if err != nil {
					return err
				}
				need := requiredPages(uint64(d.Offset) + uint64(len(bytes)))
				if need > m.MinPages {
					m.MinPages = need
				}
			}
			if m.MaxPages != nil && *m.MaxPages < m.MinPages {
				return fmt.Errorf(
					"imported memory %s.%s: recorded initial contents need %d page(s) "+
						"but the recording declares a maximum of %d",
					m.Module, m.Name, m.MinPages, *m.MaxPages)
			}
		}

		binary, err := spec.encode()
		if err != nil {
			return fmt.Errorf("building the host-state provider for module %q: %w", name, err)
		}
		// The provider must present itself under the name the guest
		// imports from, which is a module-config override rather than
		// anything in the synthesised binary.
		mod, err := r.opts.Runtime.InstantiateWithConfig(ctx, binary,
			wazero.NewModuleConfig().WithName(name))
		if err != nil {
			return fmt.Errorf("instantiating the host-state provider for module %q: %w", name, err)
		}
		r.providers[name] = mod
	}
	return nil
}

// applyInitialMemory writes the recorded initial contents into every
// imported memory (spec §3.3).
func (r *replayer) applyInitialMemory() error {
	state := r.opts.Recording.HostState
	if state == nil {
		return nil
	}
	for _, m := range state.Initial.Memories {
		mem, err := r.importedMemory(m.Module, m.Name)
		if err != nil {
			return err
		}
		for _, d := range m.Data {
			bytes, err := d.decode()
			if err != nil {
				return err
			}
			if !mem.Write(d.Offset, bytes) {
				return fmt.Errorf(
					"applying spec §3.3 initial state: writing %d byte(s) at offset %d "+
						"of imported memory %s.%s is out of range (memory is %d bytes)",
					len(bytes), d.Offset, m.Module, m.Name, mem.Size())
			}
		}
	}
	return nil
}

func (r *replayer) importedMemory(module, name string) (api.Memory, error) {
	mod, ok := r.providers[module]
	if !ok {
		return nil, fmt.Errorf(
			"no host-state provider was built for module %q; the recording's "+
				"initial state and its memory list disagree", module)
	}
	mem := mod.ExportedMemory(name)
	if mem == nil {
		return nil, fmt.Errorf("host-state provider %q exports no memory named %q", module, name)
	}
	return mem, nil
}

// serviceImport is the generated stub body: on the n-th call to an import,
// assert the recorded arguments match and return the recorded results
// (spec §6.2).
//
// It cannot return an error — wazero host functions signal failure by
// panicking — so a divergence is stashed on the replayer and the stack is
// left zeroed. `run` reports the stashed error. Panicking would abort the
// interpreter mid-trace and lose the diagnostic behind a trap message.
func (r *replayer) serviceImport(plan *importPlan, mod api.Module, stack []uint64) {
	if r.err != nil {
		zeroStack(stack, len(plan.resultTyps))
		return
	}

	// An import whose signature is `() -> ()` contributes no value runs to
	// a browser `.ct`, and `browser_session.js` emits no Call/Return record
	// for an import, so in a recording made before M39 the crossing left
	// only a pair of realm markers spelled exactly like an export's and
	// `LoadRecording` recovered nothing for it: there is nothing to check
	// against and nothing to feed back. Such a call is serviced as a no-op
	// and counted separately so the caller can report it.
	//
	// From M39 the markers name the import edge, the crossing IS recovered,
	// and this arm is skipped — the call falls through to exactly the same
	// checks every other import call gets, so its index and its position in
	// the interleaving are verified and a mismatch is a divergence.
	if len(plan.sig.Params) == 0 && len(plan.sig.Results) == 0 &&
		!r.opts.Recording.MarkersIdentifyImports {
		r.result.UncheckedImportCalls++
		return
	}

	crossings := r.opts.Recording.Crossings
	if r.cursor >= len(crossings) {
		r.err = &DivergenceError{
			What:     "import call",
			Where:    plan.describe(),
			Recorded: fmt.Sprintf("nothing — the recording ends after %d crossing(s)", len(crossings)),
			Actual:   "replay called this import",
			Detail: "the module crossed the boundary more times than it did when " +
				"recorded",
		}
		zeroStack(stack, len(plan.resultTyps))
		return
	}

	c := &crossings[r.cursor]
	if c.Kind != CrossingImport {
		r.err = &DivergenceError{
			What:     "crossing kind",
			Where:    fmt.Sprintf("crossing #%d", c.Seq),
			Recorded: c.Describe(),
			Actual:   fmt.Sprintf("replay called %s", plan.describe()),
		}
		zeroStack(stack, len(plan.resultTyps))
		return
	}
	if c.Index != plan.index {
		r.err = &DivergenceError{
			What:     "import index",
			Where:    fmt.Sprintf("crossing #%d", c.Seq),
			Recorded: fmt.Sprintf("import #%d", c.Index),
			Actual:   fmt.Sprintf("import #%d (%s.%s)", plan.index, plan.module, plan.name),
			Detail: "replay reached a different host call than the one recorded at " +
				"this point",
		}
		zeroStack(stack, len(plan.resultTyps))
		return
	}

	// --- arguments: assert the recorded values match ---------------------
	wantArgs, err := decodeTuple("argument", c.Args, plan.sig.Params)
	if err != nil {
		r.err = fmt.Errorf("%s at %s: %w", plan.describe(), c.Describe(), err)
		zeroStack(stack, len(plan.resultTyps))
		return
	}
	gotArgs := make([]Value, len(plan.sig.Params))
	for i, t := range plan.sig.Params {
		gotArgs[i] = FromWazero(t, stack[i])
	}
	for i := range wantArgs {
		if !wantArgs[i].Equal(gotArgs[i]) {
			r.err = &DivergenceError{
				What:     fmt.Sprintf("import argument %d", i),
				Where:    fmt.Sprintf("%s, %s", c.Describe(), plan.describe()),
				Recorded: wantArgs[i].String(),
				Actual:   gotArgs[i].String(),
				Detail: fmt.Sprintf("full recorded tuple %s, replayed tuple %s",
					formatValues(wantArgs), formatValues(gotArgs)),
			}
			zeroStack(stack, len(plan.resultTyps))
			return
		}
	}

	// --- spec §3.4: host mutations made while servicing this call --------
	//
	// Applied before the results are handed back, because the module
	// observes the written memory and the returned value as one outcome.
	if err := r.applyMutations(c.Seq); err != nil {
		r.err = fmt.Errorf("applying spec §3.4 host mutations at %s: %w", c.Describe(), err)
		zeroStack(stack, len(plan.resultTyps))
		return
	}

	// --- results: feed the recorded values back --------------------------
	if len(plan.sig.Results) > 0 && !c.hasResults {
		r.err = &DivergenceError{
			What:     "import result",
			Where:    fmt.Sprintf("%s, %s", c.Describe(), plan.describe()),
			Recorded: "no result values",
			Actual:   fmt.Sprintf("the import returns %d value(s) %s", len(plan.sig.Results), plan.sig),
			Detail: "an imported call's results ARE the non-determinism replay has " +
				"to feed back (spec §3.2); without them the recording is not a " +
				"re-execution input",
		}
		zeroStack(stack, len(plan.resultTyps))
		return
	}
	results, err := decodeTuple("result", c.Results, plan.sig.Results)
	if err != nil {
		r.err = fmt.Errorf("%s at %s: %w", plan.describe(), c.Describe(), err)
		zeroStack(stack, len(plan.resultTyps))
		return
	}
	for i, v := range results {
		stack[i] = v.Bits
	}

	r.cursor++
	r.result.ImportCalls++
}

// applyMutations applies the spec §3.4 host writes anchored to one crossing.
func (r *replayer) applyMutations(seq int) error {
	state := r.opts.Recording.HostState
	for _, mu := range state.MutationsFor(seq) {
		for _, w := range mu.MemoryWrites {
			mem, err := r.importedMemory(w.Module, w.Name)
			if err != nil {
				return err
			}
			bytes, err := w.decode()
			if err != nil {
				return err
			}
			if !mem.Write(w.Offset, bytes) {
				return fmt.Errorf(
					"writing %d byte(s) at offset %d of imported memory %s.%s is "+
						"out of range (memory is %d bytes)",
					len(bytes), w.Offset, w.Module, w.Name, mem.Size())
			}
		}
		for _, s := range mu.GlobalSets {
			mod, ok := r.providers[s.Module]
			if !ok {
				return fmt.Errorf(
					"no host-state provider was built for module %q", s.Module)
			}
			g := mod.ExportedGlobal(s.Name)
			if g == nil {
				return fmt.Errorf(
					"host-state provider %q exports no global named %q", s.Module, s.Name)
			}
			mg, ok := g.(api.MutableGlobal)
			if !ok {
				return fmt.Errorf("imported global %s.%s is not mutable", s.Module, s.Name)
			}
			v, err := s.decode()
			if err != nil {
				return err
			}
			mg.Set(v.Bits)
		}
	}
	return nil
}

// callExport invokes one recorded exported call and validates its return
// value against the recording (spec §6.4 and §3.1).
func (r *replayer) callExport(ctx context.Context, guest api.Module, c *Crossing) error {
	if r.cursor >= len(r.opts.Recording.Crossings) || r.cursor != c.Seq {
		// The driver walks top-level exports in order, so the cursor must
		// already be sitting on this crossing. If it is not, the module
		// consumed a different number of import crossings than recorded.
		var recorded string
		if r.cursor < len(r.opts.Recording.Crossings) {
			recorded = r.opts.Recording.Crossings[r.cursor].Describe()
		} else {
			recorded = "nothing — the recording is exhausted"
		}
		return &DivergenceError{
			What:     "crossing order",
			Where:    c.Describe(),
			Recorded: recorded,
			Actual:   fmt.Sprintf("replay is about to make %s", c.Describe()),
			Detail: "the module made a different number of host calls than it did " +
				"when recorded, so the two crossing streams no longer line up",
		}
	}

	fn := guest.ExportedFunction(c.Name)
	if fn == nil {
		return fmt.Errorf("%s: the instantiated module exports no function %q", c.Describe(), c.Name)
	}
	sig, err := signatureOf(fn.Definition())
	if err != nil {
		return fmt.Errorf("export %q: %w", c.Name, err)
	}

	args, err := decodeTuple("argument", c.Args, sig.Params)
	if err != nil {
		return fmt.Errorf("%s: %w", c.Describe(), err)
	}
	raw := make([]uint64, len(args))
	for i, v := range args {
		raw[i] = v.Bits
	}

	r.cursor++
	r.result.ExportCalls++

	got, callErr := fn.Call(ctx, raw...)
	// A divergence detected inside a stub is reported in preference to
	// whatever the call itself did: the stub error is the cause, the call
	// failure (if any) the symptom.
	if r.err != nil {
		return r.err
	}
	if callErr != nil {
		return fmt.Errorf("%s: replaying %q trapped: %w", c.Describe(), c.Name, callErr)
	}

	// --- spec §3.1 / §6: check the exported return value ------------------
	//
	// The return value is not needed to drive re-execution — it is
	// reproduced by re-executing — but comparing it against the recording
	// is the cheapest possible check that the replay stayed faithful.
	want, err := decodeTuple("result", c.Results, sig.Results)
	if err != nil {
		return fmt.Errorf("%s: %w", c.Describe(), err)
	}
	if len(got) != len(want) {
		return &DivergenceError{
			What:     "exported return arity",
			Where:    c.Describe(),
			Recorded: fmt.Sprintf("%d value(s)", len(want)),
			Actual:   fmt.Sprintf("%d value(s)", len(got)),
		}
	}
	for i := range want {
		actual := FromWazero(sig.Results[i], got[i])
		if !want[i].Equal(actual) {
			return &DivergenceError{
				What:     fmt.Sprintf("exported return value %d", i),
				Where:    fmt.Sprintf("%s, %s%s", c.Describe(), c.Name, sig),
				Recorded: want[i].String(),
				Actual:   actual.String(),
				Detail:   fmt.Sprintf("called with %s", formatValues(args)),
			}
		}
	}
	return nil
}

// zeroStack clears the result slots of a host-function stack so a
// short-circuited stub leaves the module with defined values rather than
// the incoming arguments.
func zeroStack(stack []uint64, results int) {
	for i := 0; i < results && i < len(stack); i++ {
		stack[i] = 0
	}
}
