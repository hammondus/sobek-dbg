# Design decisions

Experiment: line-by-line VS Code debugging for scripts embedded in Go via an
**unmodified upstream engine** — `github.com/grafana/sobek` (see the next
section for why sobek). The alternative considered (and prototyped by others)
is an engine with a debugger compiled into the VM (speakeasy-api/goja
`feat/debugger`, upstream PR dop251/goja#702 — stalled). This repo tests the
fork-free route so the real project doesn't have to bet its engine dependency
on a *debugger* fork staying alive.

## Engine: sobek (Grafana's maintained goja fork)

Started on `dop251/goja`; switched to `github.com/grafana/sobek` once the
question was asked properly. Sobek is Grafana's fork of goja, maintained for
k6 (which runs on it in production), tracks upstream goja commits, and adds
ES module support — directly relevant to future multi-file scripts, where
real `import` beats a `scripts.call()` host function. goja itself is a
single-maintainer project; sobek has a company whose product depends on it.

The swap was a drop-in: sobek preserves goja's package layout (`parser`,
`ast`, `file`) and the two load-bearing internals — direct-eval scope
materialization and `CaptureCallStack` — behave identically (verified by the
full test suite, which encodes both). The project and its wiring were later
renamed to match the engine (`gojadbg` → `sobekdbg`, debug type `goja` →
`sobek`) — one name across module, extension, and launch config.

Also rejected: v8go (cgo — cross-compiling to linux/arm64 becomes painful, a
huge opaque C++ blob, and its Go bindings don't expose the V8 inspector, so
V8's debugger isn't actually reachable) and non-JS languages (Lua, Starlark —
lose the one-language-across-stack property with the browser frontend and the
tsserver/JSDoc tooling story).

## Source instrumentation instead of a VM fork

In debug mode the script is rewritten before compiling: a hook call is
spliced in front of every statement. Production runs the original source —
zero overhead, zero divergence from upstream sobek, and the approach survives
every sobek upgrade because it only touches `sobek/parser` + `sobek/ast`
(public, stable packages).

Rules that keep the rewrite honest:

- **Text splicing, not AST re-generation.** sobek has no code generator, and
  regenerating source would destroy positions. Insertions are offset-sorted
  strings containing no newlines, so every line number in stack traces and
  breakpoints matches the file on disk. Columns *do* shift; the instrumenter
  records every splice (line, original column, length) and `Result.OrigCol`
  maps frame columns back — a column inside injected text clamps to the
  statement the injection fronts, which is exactly where the caret belongs
  when paused inside the hook call itself.
- Single-statement bodies (`if (c) x();`) are brace-wrapped so the hook
  doesn't change what the construct governs. The closing brace absorbs a
  trailing `;` — otherwise `if (c) {x()}; else` is a syntax error. The scan
  to that `;` skips whitespace *including newlines* and comments (skipped,
  never consumed — no-newline splicing still holds), because the parser
  accepts `if (c) x()\n; else` and `if (c) x() /*…*/ ; else`.
- Directive prologues (`"use strict"`) stay first — but only where the
  grammar has prologues: program and function bodies. A leading string
  literal in a plain block or switch case is an ordinary statement and is
  hooked like any other.
- `else if` is treated as a continuation of the same statement (also forced:
  sobek's parser leaves `Idx0 == 0` on those nodes).
- `debugger;` statements compile to a forced-stop hook.

## The eval thunk: `__dbg(sid, line, scopeID, __x => eval(__x))`

The third hook argument is the whole reason this works. Direct `eval` inside
an arrow gives the Go side an evaluator *inside the paused scope*: reading
locals, conditional breakpoints, watch expressions, and **writing variables**
(`total = 100` from the debug console sticks) all come for free. sobek
implements the spec behaviour where the presence of direct eval forces
enclosing scopes to stay materialized, so locals are reliably visible —
verified before anything else was built (including strict mode and TDZ
behaviour: premature access throws a catchable ReferenceError, which the
variables view filters out).

Variable *names* can't be enumerated at runtime, so the instrumenter computes
them statically per statement (`scopeID` indexes a table). Name resolution is
deliberately approximate — function-granularity, over-collecting — because a
wrong name simply fails its guarded eval and is dropped, never shown wrong.

## Stack traces from `Runtime.CaptureCallStack`

The hook is a Go callback, so sobek's own `CaptureCallStack` yields real
frames (function name, file, line) at every pause. This kills any need to
instrument function entry/exit; step-over/out are implemented purely as
"run until hook with depth ≤/< saved depth".

## Pausing = blocking the VM goroutine

sobek runtimes are single-goroutine. The hook *is* the pause: it parks in a
`select`, servicing evaluation requests from the DAP goroutine over a
channel. The DAP side never touches the runtime, which makes the race
detector happy by construction.

Two rules keep the pause/disconnect handoff sound:

- **Teardown clears the sink first, then resumes** (`detach`), and the hook
  re-checks the sink under the mutex immediately before committing to a
  pause. The mutex serializes the two, so a hook either sees the sink gone
  and runs on, or has already set `paused` and is guaranteed to receive the
  resume — the VM can never park orphaned with its `stopped` event dropped.
  `detach` also cancels pending step/pause state so it can't fire as a
  spurious stop in the next session.
- **Channel sends to the VM goroutine are bounded** (2s, like the eval
  timeouts). If the VM is stuck inside a debug-console eval that never
  returns (`while(true){}`), `paused` stays true but nothing is receiving;
  an unbounded `resumeCh` send would wedge the DAP dispatch goroutine — and
  with it the whole session, disconnect included. The timed-out resume is
  dropped: nothing can rescue a VM executing user JS that never yields
  (sobek's `Interrupt` could, but that kills the script — not a debugger's
  call to make), so the session stays responsive and the user retries.

## Blocked host calls are pause points (`HostBlocked`)

Hooks only run between statements, so while the VM goroutine is inside a
blocking host function (a dialog wait, a sleep, a job join) no hook fires and
a pause request cannot take effect until the call returns. For the intended
workload — scripts that spend most of their wall-clock time blocked in host
calls — that would make Pause appear dead exactly when it matters most.

The fix is a contract, not a special case: a host function that blocks must
start its real work on another goroutine and park through
`HostBlocked(label, done)` on the VM goroutine. `HostBlocked` waits for
`done`, but a pause request pokes it into committing a real pause — same
`stopped` event, same service loop as a hook pause. Two facts make this
sound:

- **The stack is capturable**: `HostBlocked` runs on the VM goroutine
  mid-call, so `CaptureCallStack` yields the real frames, topmost at the
  statement that made the host call.
- **Evaluation works**: every hook stashes its eval thunk before the fast
  path returns. The statement containing the host call has not finished, so
  its direct-eval scope is still materialized — reading *and writing* locals
  works while paused inside the call.

Continue (or a step) returns the VM to waiting; the host operation itself is
never interrupted — pausing must not change what the script computes. A step
request simply lands at the next hook after the call returns, which is the
next statement. If the pause request races `HostBlocked`'s registration, the
pending `pauseReq` is checked at entry; if it races completion, the request
degrades to the pre-existing behaviour (pause at the next hook). Host calls
made *from a debug-console eval* skip the machinery entirely (`inEval`) —
re-entering the pause loop from inside a pause would nest parks; the DAP
side's eval timeout bounds them instead.

`HostBlocked` is nil-receiver-safe so hosts can call it unconditionally;
with no debugger it degrades to a plain channel wait.

## Script-calling-script: same VM, nested `RunProgram`

`CallScript(path)` runs another instrumented file in the same runtime, from a
host binding, with these stepping semantics: **step-over free-runs the
sub-script** (its breakpoints still hit) and stops at the caller's next
statement; **step-in** stops at the sub-script's first statement; **step-out**
from inside a sub-script returns to the caller. None of that needed new
stepping code — sobek's interpreter stack spans a nested `RunProgram`
(verified empirically before building on it), so a sub-script's statements
are simply deeper than the call site and the existing depth comparisons
produce exactly those rules.

What multi-script *did* require:

- **The hook identifies its file**: the instrumenter now bakes a caller-chosen
  `scriptID` into every hook as its first argument, so the free-running fast
  path can find its breakpoint map and scope table without capturing a stack.
- **Step state carries file identity**: "same statement" is depth + line +
  script — two files can have a statement on the same line number.
- **Breakpoints are keyed by canonical path** (absolute, symlinks resolved,
  case-folded). `setBreakpoints` for a not-yet-loaded file loads it eagerly —
  instrument+compile touch no VM state — so its lines verify correctly and
  the program is pre-cached for the first `CallScript`.

The same-VM choice is deliberate: a direct call is synchronous composition,
like a function call, which is what makes step-into coherent at all. The
cost is shared globals — a sub-script's `var`s land in the caller's global
scope. That is accepted (and listed under limitations); *isolation* is what
detached fresh-VM jobs are for, not direct calls. A sub-script failure
surfaces in the caller as a catchable exception via the host binding.

## Hand-rolled DAP server (no `google/go-dap`)

The wire format is Content-Length-framed JSON (same as LSP) and only ~15
request types are needed; the whole server is one file of stdlib code.
`go-dap` would be a defensible dependency (Google, used by Delve) if the
message surface grows — noted as the escape hatch, not the default.

sobek stays the only dependency.

Server policies worth knowing:

- **One client at a time, newest wins.** A fresh attach kicks any existing
  connection rather than queueing behind it in the listen backlog — the
  existing one may be a half-dead socket the OS won't time out for hours,
  and a debugger that silently ignores a reattach is worse than one that
  drops a session the user has abandoned. The kicked handler's teardown
  checks it is still the active connection before touching shared state.
- **Everything spliced into an eval is comment-proofed.** The render
  template, `setVariable`, and breakpoint conditions all close their
  wrapping paren on a fresh line, so an expression ending in `// comment`
  can't swallow it. `setVariable` additionally insists the name is a plain
  identifier — it comes back from the client verbatim, not from our
  variables list.
- **Paths are compared through `EvalSymlinks`** (then case-folded): the
  editor and the host frequently reach the same file via different links
  (macOS `/tmp` → `/private/tmp`), and a silent mismatch discards every
  breakpoint. This assumes editor and host share a filesystem — see the
  limitation below for the remote case.
- Script failure is reported as `exited` with code 1, not 0.

## VS Code glue

VS Code only speaks DAP through a registered debug type, so `vscode-ext/` is
a ~15-line extension mapping type `sobek` to a `DebugAdapterServer` TCP
connection (`make install-ext`). No adapter process, no logic. The
`debugServer` launch.json escape hatch was rejected: it's documented for
extension development and still requires a registered type.

## Plain JS + JSDoc, not TypeScript

Scripts are authored in the exact bytes sobek executes. That is the invariant
the whole debugger rests on (no inserted newlines, `OrigCol` for columns), and
TypeScript would break it: sobek can't run TS, so TS means a transpile step,
which means the on-disk file no longer matches the executing source, which
means source maps threaded through both the instrumenter and the DAP server —
a second position-translation system on top of the one we already have.

Instead, typing is an editor-only concern: `jsconfig.json` at the repo root
turns on `checkJs`/`strict` for tsserver, and `types/host.d.ts` declares the
host API (`log` today; `db`/`ui`/`jobs` as the planned surface) so scripts get
IntelliSense and checking against it. Nothing at build or run time reads
either file; there is no Node toolchain in the loop. The `.d.ts` must be kept
in lockstep with the Go bindings by hand — a declared-but-unbound name fails
only at runtime.

If plain JSDoc ever grates, the revisit path is position-preserving type
stripping (the `ts-blank-space` approach: types blanked to whitespace, line
*and* column positions survive) — not full transpilation with source maps.

## Known limitations (acceptable for the prototype)

- Concise arrow bodies (`x => x * 2`) have no statement to hook; stepping
  doesn't stop inside. Write braces to debug an arrow.
- Variables/evaluate work in the **top frame only** — the eval thunk lives
  where execution paused. Outer frames show source position but not locals.
- Objects render as flat JSON strings, not expandable trees (an expansion
  scheme via `variablesReference` → path evals is the obvious upgrade).
- `CallScript` sub-scripts run in the caller's global scope: their `var`s
  are (and see) the caller's globals. Deliberate for direct calls; isolation
  belongs to detached fresh-VM jobs.
- One VM (one thread) per debugger instance; concurrently running scripts
  need runs-as-DAP-threads bookkeeping in `dbg`.
- `for`-loop head (init/test/update) isn't a step stop; the loop line hits
  once per entry.
- Scope names are function-granular: a block-scoped `let` may be listed
  (and TDZ-filtered) outside its block.
- Hook cost is real (a Go call + closure per statement) — debug mode only,
  by design; `-nodebug` runs pristine source.
- The names `__dbg` and `__x` are reserved in debugged scripts.
- **Editor and host must share a filesystem.** Breakpoint paths are matched
  by resolving symlinks and case-folding, which cannot relate a laptop path
  to a server path. Debugging a host running elsewhere needs
  `localRoot`/`remoteRoot` mapping applied in both directions (README
  porting item 4). The DAP transport itself is already remote-capable.
