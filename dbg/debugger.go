// Package dbg is a step debugger for scripts running in an unmodified sobek
// runtime. It relies on instrument to inject a hook call before every
// statement; the hook — running on the VM goroutine — decides whether to
// pause. While paused it services evaluation requests over a channel, so the
// DAP goroutine never touches the sobek runtime directly (sobek runtimes are
// not goroutine-safe).
package dbg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/grafana/sobek"

	"sobekdbg/instrument"
)

type mode int

const (
	modeRun mode = iota
	modeStepIn
	modeStepOver
	modeStepOut
)

type breakpoint struct {
	condition string // empty = unconditional
}

type evalReq struct {
	expr  string
	reply chan evalResp
}

type evalResp struct {
	val sobek.Value
	err error
}

// script is one loaded (instrumented, compiled) file. Its id is baked into
// every hook the instrumenter emitted for it, so the hook can find its own
// breakpoint map and scope table without capturing a stack.
type script struct {
	id   int
	path string // absolute path, as compiled (what stack frames report)
	key  string // canonical form of path (symlinks resolved, case-folded)
	info *instrument.Result
	prog *sobek.Program
}

type Debugger struct {
	mu sync.Mutex

	vm      *sobek.Runtime
	scripts []*script          // indexed by script.id
	byPath  map[string]*script // keyed by script.key

	bps         map[string]map[int]breakpoint // script key -> line -> bp
	mode        mode
	fromDepth   int
	fromLine    int
	fromKey     string // script key of the statement a step started from
	pauseReq    bool
	stopOnEntry bool
	started     bool
	inEval      bool // suppress reentrant hooks while evaluating user expressions

	// Snapshot of the paused state; valid only while paused.
	paused  bool
	stack   []sobek.StackFrame
	scope   []string
	curLine int
	thunk   sobek.Callable

	// Last hook site, stashed on every hook (even free-running) so a blocked
	// host call can pause with a working eval thunk: the statement containing
	// the call is still live, so its direct-eval scope is too.
	lastThunk   sobek.Value
	lastScript  *script
	lastLine    int
	lastScopeID int

	// Non-nil while the VM goroutine waits inside HostBlocked; a pause
	// request pokes it to convert the wait into a real pause.
	blockedPoke chan struct{}

	resumeCh chan mode
	evalCh   chan evalReq

	events func(event string, body map[string]any) // nil until a DAP client attaches

	configured     chan struct{}
	configuredOnce sync.Once
}

func New() *Debugger {
	return &Debugger{
		byPath:     map[string]*script{},
		bps:        map[string]map[int]breakpoint{},
		resumeCh:   make(chan mode),
		evalCh:     make(chan evalReq),
		configured: make(chan struct{}),
	}
}

// Load reads, instruments and compiles the script at path, registering it
// under the next script ID; loading the same file again returns the cached
// program. The absolute path is used as the compile filename so stack frames
// — and therefore DAP stack-trace sources — point at the file the editor has
// open. Safe from any goroutine (it never touches the VM).
func (d *Debugger) Load(path string) (*sobek.Program, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	key := canonPath(abs)

	// Instrument+compile under the lock: it serializes ID allocation, and the
	// work is pure (no VM access), so holding the lock through a rare load is
	// simpler than a reserve-then-fill dance.
	d.mu.Lock()
	defer d.mu.Unlock()
	if s := d.byPath[key]; s != nil {
		return s.prog, nil
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	id := len(d.scripts)
	res, err := instrument.File(abs, string(src), id)
	if err != nil {
		return nil, err
	}
	prog, err := sobek.Compile(abs, res.Source, false)
	if err != nil {
		return nil, fmt.Errorf("instrumented source failed to compile (instrumenter bug): %w", err)
	}
	s := &script{id: id, path: abs, key: key, info: res, prog: prog}
	d.scripts = append(d.scripts, s)
	d.byPath[key] = s
	return prog, nil
}

// CallScript loads (cached) and runs the script at path in the attached VM —
// the building block for a script-calls-script host binding. Must run on the
// VM goroutine. The interpreter stack spans the nested run, so the standard
// depth-based stepping gives call-boundary semantics for free: step-over
// free-runs the sub-script, step-in stops at its first statement, step-out
// from inside it returns to the parent.
func (d *Debugger) CallScript(path string) (sobek.Value, error) {
	prog, err := d.Load(path)
	if err != nil {
		return nil, err
	}
	return d.vm.RunProgram(prog)
}

// Attach installs the debug hook into the runtime. Must be called before
// running an instrumented program; the runtime must only be driven from one
// goroutine, as usual with sobek.
func (d *Debugger) Attach(vm *sobek.Runtime) error {
	d.vm = vm
	return vm.Set(instrument.HookName, d.hook)
}

// WaitConfigured blocks until a DAP client has finished configuration
// (breakpoints set), or the timeout elapses. Use it to hold script start
// until the debugger is ready.
func (d *Debugger) WaitConfigured(timeout time.Duration) bool {
	select {
	case <-d.configured:
		return true
	case <-time.After(timeout):
		return false
	}
}

// Done reports script completion to the client. Call after RunProgram.
func (d *Debugger) Done(err error) {
	code := 0
	if err != nil {
		d.Output("stderr", "script error: "+err.Error()+"\n")
		code = 1
	}
	d.emit("terminated", map[string]any{})
	d.emit("exited", map[string]any{"exitCode": code})
}

// Output forwards host output (e.g. a console.log binding) to the client.
func (d *Debugger) Output(category, text string) {
	d.emit("output", map[string]any{"category": category, "output": text})
}

func (d *Debugger) emit(event string, body map[string]any) {
	d.mu.Lock()
	ev := d.events
	d.mu.Unlock()
	if ev != nil {
		ev(event, body)
	}
}

// hook is the function every instrumented statement calls:
// __dbg(scriptID, line, scopeID, __x => eval(__x) [, forced]).
// It runs on the VM goroutine.
func (d *Debugger) hook(call sobek.FunctionCall) sobek.Value {
	sid := int(call.Argument(0).ToInteger())
	line := int(call.Argument(1).ToInteger())
	scopeID := int(call.Argument(2).ToInteger())
	forced := len(call.Arguments) > 4

	d.mu.Lock()
	if d.inEval {
		d.mu.Unlock()
		return sobek.Undefined()
	}
	if sid < 0 || sid >= len(d.scripts) {
		d.mu.Unlock()
		return sobek.Undefined() // hook from a script this debugger didn't load
	}
	sc := d.scripts[sid]
	// Stashed regardless of client state: a client may attach while the VM is
	// blocked in a host call, and the pause then needs the last live thunk.
	d.lastThunk = call.Argument(3)
	d.lastScript = sc
	d.lastLine = line
	d.lastScopeID = scopeID
	if d.events == nil {
		d.mu.Unlock()
		return sobek.Undefined()
	}
	entry := false
	if !d.started {
		d.started = true
		entry = d.stopOnEntry
	}
	bp, hasBP := d.bps[sc.key][line]
	stepping := d.mode != modeRun
	pause := d.pauseReq
	d.mu.Unlock()

	if !forced && !entry && !hasBP && !stepping && !pause {
		return sobek.Undefined() // fast path: free-running
	}

	thunk, ok := sobek.AssertFunction(call.Argument(3))
	if !ok {
		return sobek.Undefined()
	}

	frames := d.jsFrames()
	depth := len(frames)

	reason := ""
	switch {
	case forced:
		reason = "debugger statement"
	case entry:
		reason = "entry"
	case pause:
		reason = "pause"
	case hasBP && d.conditionHit(bp, thunk):
		reason = "breakpoint"
	case stepping && d.stepHit(depth, line, sc.key):
		reason = "step"
	}
	if reason == "" {
		return sobek.Undefined()
	}

	d.mu.Lock()
	// Re-check the sink before committing to the pause: the client may have
	// disconnected since the check at the top of the hook (detach clears the
	// sink first, then resumes — so seeing a sink here means a detach that
	// started after this point will still deliver its resume).
	if d.events == nil {
		d.mu.Unlock()
		return sobek.Undefined()
	}
	d.paused = true
	d.stack = frames
	d.curLine = line
	d.thunk = thunk
	d.scope = nil
	if scopeID >= 0 && scopeID < len(sc.info.Scopes) {
		d.scope = sc.info.Scopes[scopeID]
	}
	d.pauseReq = false
	d.mode = modeRun // avoid re-triggering while we sit here
	d.mu.Unlock()

	d.emit("stopped", map[string]any{
		"reason": reason, "threadId": 1, "allThreadsStopped": true,
	})
	d.park(thunk, depth, line, sc.key)
	return sobek.Undefined()
}

// park is the service loop: the VM goroutine sits here, which *is* the
// pause, answering evaluation requests until a resume delivers a new mode.
func (d *Debugger) park(thunk sobek.Callable, depth, line int, key string) {
	for {
		select {
		case req := <-d.evalCh:
			req.reply <- d.eval(thunk, req.expr)
		case m := <-d.resumeCh:
			d.mu.Lock()
			d.mode = m
			d.fromDepth = depth
			d.fromLine = line
			d.fromKey = key
			d.paused = false
			d.mu.Unlock()
			return
		}
	}
}

// HostBlocked waits for done on behalf of a blocking host function while
// keeping the debugger responsive: a pause request arriving mid-block stops
// the script at the statement that made the host call — with working
// evaluation — and continue returns it to waiting. Without this, a pause
// during a blocked call could not take effect until the call returned (hooks
// only run between statements), which for host-call-heavy scripts is most of
// their wall-clock time.
//
// Contract: call on the VM goroutine, with the real work already started on
// another goroutine:
//
//	done := make(chan struct{})
//	go func() { defer close(done); /* blocking work */ }()
//	d.HostBlocked("ui.showDialog", done)
//
// The host operation itself is never interrupted — pausing must not change
// what the script computes. Safe on a nil *Debugger (production mode / no
// debugger wired): it just waits.
func (d *Debugger) HostBlocked(label string, done <-chan struct{}) {
	if d == nil {
		<-done
		return
	}
	poke := make(chan struct{}, 1)
	d.mu.Lock()
	if d.inEval {
		// The host call came from a debug-console eval while already paused.
		// Re-entering the pause machinery from here would nest parks; just
		// wait (the DAP side bounds the eval with its own timeout).
		d.mu.Unlock()
		<-done
		return
	}
	d.blockedPoke = poke
	// A pause requested in the window between the statement's hook and this
	// registration would otherwise sit until the host call returns.
	pending := d.pauseReq
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.blockedPoke = nil
		d.mu.Unlock()
	}()
	if pending {
		d.parkBlocked(label)
	}
	for {
		select {
		case <-done:
			return
		case <-poke:
			d.parkBlocked(label)
		}
	}
}

// parkBlocked commits a pause from inside a blocked host call. The stack is
// captured live; the eval thunk is the one stashed by the statement's own
// hook, whose scope is still materialized because the statement hasn't
// finished executing.
func (d *Debugger) parkBlocked(label string) {
	frames := d.jsFrames()
	d.mu.Lock()
	thunk, ok := sobek.AssertFunction(d.lastThunk)
	if d.events == nil || !d.pauseReq || !ok || d.lastScript == nil {
		// No client, the pause was cancelled while the poke was in flight, or
		// no hook has run yet (no thunk to evaluate with): stay blocked.
		d.mu.Unlock()
		return
	}
	sc := d.lastScript
	line := d.lastLine
	if len(frames) > 0 {
		// Prefer the live position: a multi-line statement can put the call
		// on a later line than the statement's hook.
		line = frames[0].Position().Line
	}
	d.paused = true
	d.stack = frames
	d.curLine = line
	d.thunk = thunk
	d.scope = nil
	if d.lastScopeID >= 0 && d.lastScopeID < len(sc.info.Scopes) {
		d.scope = sc.info.Scopes[d.lastScopeID]
	}
	d.pauseReq = false
	d.mode = modeRun
	d.mu.Unlock()

	d.emit("stopped", map[string]any{
		"reason": "pause", "threadId": 1, "allThreadsStopped": true,
		"description": "Paused in host call " + label,
	})
	d.park(thunk, len(frames), line, sc.key)
}

func (d *Debugger) eval(thunk sobek.Callable, expr string) evalResp {
	d.mu.Lock()
	d.inEval = true
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.inEval = false
		d.mu.Unlock()
	}()
	v, err := thunk(sobek.Undefined(), d.vm.ToValue(expr))
	return evalResp{v, err}
}

func (d *Debugger) conditionHit(bp breakpoint, thunk sobek.Callable) bool {
	if bp.condition == "" {
		return true
	}
	// Newline before the close paren: a condition ending in a line comment
	// must not swallow it (same guard as the DAP render template).
	res := d.eval(thunk, "("+bp.condition+"\n)")
	if res.err != nil {
		return true // a broken condition should be visible, not silently skipped
	}
	return res.val.ToBoolean()
}

// stepHit decides whether a hook at (depth, line, key) completes the pending
// step. "Same statement" means same depth, same line *and* same file — two
// scripts can have a statement on the same line number. A nested CallScript
// runs deeper than its caller, so step-over free-runs a sub-script and
// step-out from inside one returns to the parent, with no extra cases.
func (d *Debugger) stepHit(depth, line int, key string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	samePos := line == d.fromLine && key == d.fromKey
	switch d.mode {
	case modeStepIn:
		return depth != d.fromDepth || !samePos
	case modeStepOver:
		return depth < d.fromDepth || (depth == d.fromDepth && !samePos)
	case modeStepOut:
		return depth < d.fromDepth
	}
	return false
}

// jsFrames returns the JS portion of the call stack (the hook's own native
// frame trimmed off), innermost first.
func (d *Debugger) jsFrames() []sobek.StackFrame {
	var out []sobek.StackFrame
	for _, f := range d.vm.CaptureCallStack(0, nil) {
		if f.SrcName() == "<native>" {
			continue
		}
		out = append(out, f)
	}
	return out
}

// --- accessors used by the DAP server (never touch the sobek runtime) ---

func (d *Debugger) setEventSink(f func(string, map[string]any)) {
	d.mu.Lock()
	d.events = f
	d.mu.Unlock()
}

// setBreakpoints replaces the breakpoint set for one source file. A file this
// debugger hasn't loaded yet is loaded eagerly — instrument+compile touch no
// VM state, so it's safe from the DAP goroutine — which lets its lines verify
// correctly and pre-caches the program for a later CallScript. If it can't be
// loaded (missing file, parse error) the breakpoints are stored anyway and
// report unverified; they bind if the file loads later.
func (d *Debugger) setBreakpoints(path string, lines []int, conds []string) []bool {
	_, _ = d.Load(path)
	key := canonPath(path)
	d.mu.Lock()
	defer d.mu.Unlock()
	sc := d.byPath[key]
	m := map[int]breakpoint{}
	verified := make([]bool, len(lines))
	for i, l := range lines {
		m[l] = breakpoint{condition: conds[i]}
		verified[i] = sc != nil && sc.info.Lines[l]
	}
	d.bps[key] = m
	return verified
}

func (d *Debugger) resume(m mode) {
	d.mu.Lock()
	if !d.paused {
		if m == modeRun {
			// Continue while running also cancels any step/pause that hasn't
			// fired yet, so it can't surface as a spurious stop later.
			d.mode = modeRun
			d.pauseReq = false
			d.mu.Unlock()
			return
		}
		// A step request while running behaves like pause-at-next-statement —
		// including when "running" means blocked inside a host call.
		d.pauseReq = true
		poke := d.blockedPoke
		d.mu.Unlock()
		pokeBlocked(poke)
		return
	}
	d.mu.Unlock()
	// Bounded send: if the VM goroutine is stuck inside a user eval that never
	// returns (`while(true){}` in the debug console), paused stays true but
	// nothing is receiving. An unbounded send here would wedge the DAP
	// dispatch goroutine — and with it the whole session, disconnect included.
	select {
	case d.resumeCh <- m:
	case <-time.After(2 * time.Second):
	}
}

// detach tears down a client session: drop the event sink, cancel pending
// step/pause state, and release the VM if it is parked. Clearing the sink
// *before* resuming closes the race with a hook that is about to pause — the
// hook re-checks the sink under lock before committing, so it either sees nil
// and runs on, or has already set paused and receives the resume.
func (d *Debugger) detach() {
	d.mu.Lock()
	d.events = nil
	d.mode = modeRun
	d.pauseReq = false
	paused := d.paused
	d.mu.Unlock()
	if paused {
		select {
		case d.resumeCh <- modeRun:
		case <-time.After(2 * time.Second):
		}
	}
}

func (d *Debugger) requestPause() {
	d.mu.Lock()
	d.pauseReq = true
	poke := d.blockedPoke
	d.mu.Unlock()
	pokeBlocked(poke)
}

// pokeBlocked nudges a VM goroutine waiting in HostBlocked, if there is one.
// Buffered + non-blocking: the waiter may have just returned (its pauseReq
// still fires at the next hook), or may already have a poke pending.
func pokeBlocked(poke chan struct{}) {
	if poke == nil {
		return
	}
	select {
	case poke <- struct{}{}:
	default:
	}
}

// evalInFrame evaluates an expression in the paused top frame.
func (d *Debugger) evalInFrame(expr string) (sobek.Value, error) {
	d.mu.Lock()
	if !d.paused {
		d.mu.Unlock()
		return nil, fmt.Errorf("not paused")
	}
	d.mu.Unlock()
	req := evalReq{expr: expr, reply: make(chan evalResp, 1)}
	select {
	case d.evalCh <- req:
	case <-time.After(2 * time.Second):
		return nil, fmt.Errorf("not paused")
	}
	select {
	case res := <-req.reply:
		return res.val, res.err
	case <-time.After(5 * time.Second):
		return nil, fmt.Errorf("evaluation timed out")
	}
}

func (d *Debugger) snapshot() (frames []sobek.StackFrame, scope []string, paused bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]sobek.StackFrame(nil), d.stack...), d.scope, d.paused
}

func (d *Debugger) markConfigured() {
	d.configuredOnce.Do(func() { close(d.configured) })
}

// origColumn maps a column from a stack frame (positions in the instrumented
// source) back to the on-disk source of whichever script the frame is in.
// Lines are preserved by instrumentation; only columns shift.
func (d *Debugger) origColumn(srcName string, line, col int) int {
	d.mu.Lock()
	sc := d.byPath[canonPath(srcName)]
	d.mu.Unlock()
	if sc == nil {
		return col
	}
	return sc.info.OrigCol(line, col)
}

// canonPath is the map-key form of a script path: absolute, symlinks
// resolved (macOS /tmp -> /private/tmp, symlinked workspaces), case-folded
// (macOS default filesystems are case-insensitive — the editor may report a
// differently-cased path than the host used). A silent mismatch here
// discards every breakpoint for the file.
func canonPath(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		p = a
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return strings.ToLower(filepath.Clean(p))
}

func samePath(a, b string) bool { return canonPath(a) == canonPath(b) }
