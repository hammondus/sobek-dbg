# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`sobekdbg` — a proof of concept giving VS Code full DAP step-debugging (breakpoints,
conditional breakpoints, step in/over/out, call stack, variables, debug-console
evaluation *including assignment to locals*) for JavaScript running in an
**unmodified upstream sobek**. The whole bet is: instrument the script source in
debug mode rather than fork the VM. `DESIGN-DECISIONS.md` is the authoritative
record of why — read it before changing the instrumenter or the hook protocol.

`github.com/grafana/sobek` (Grafana's maintained fork of goja) is the only direct dependency, and keeping it that way is
a design goal, not an accident. Everything else is stdlib. Other dependencies will
be considered, but on a case by case basis. They are to be minimised unless
completely necessary. 


## Commands

```sh
make build          # go build -o bin/demo ./cmd/demo
make test           # go vet ./... && go test -race ./...
make run            # build, then ./bin/demo -wait testdata/sample.js (DAP on 127.0.0.1:4711)
make release        # cross-compiled stripped binaries into dist/
make install-ext    # copy vscode-ext/ into ~/.vscode/extensions (then reload VS Code)

go test ./dbg/ -run TestStepIntoFunction -v      # single test
go test ./instrument/ -run TestScopesContainExpectedNames -v

./bin/demo -nodebug testdata/sample.js           # production path: pristine source, no hooks
```

`docker-build`, `deploy`, and `logs` are deliberate no-op stubs — they exist only
to keep the Makefile interface identical across repos.

Manual end-to-end check: `make install-ext` once, reload VS Code, `make run`, then
F5 ("Attach to sobek demo" in `.vscode/launch.json`, `stopOnEntry` is on).

## Architecture

Three layers, each independently testable:

**`instrument/`** — `instrument.File(filename, src, scriptID)` parses with sobek's
own `parser`/`ast` and splices `;__dbg(<sid>,<line>,<scopeID>,__x=>eval(__x)[,1]);`
in front of every statement. The `sid` identifies the file to the debugger's
scripts registry without a stack capture on the fast path. Key invariants, all of
which have tests:

- **Textual splicing only, driven by byte offsets** (`file.Idx` is 1-based, offsets
  are 0-based — see `off()`). No AST regeneration; sobek has no code generator and
  regenerating would destroy positions.
- **No inserted newlines**, so line numbers in the instrumented source match the
  file on disk exactly. Breakpoints and stack traces depend on this. Columns do
  shift; every splice is recorded and `Result.OrigCol` maps frame columns back to
  the on-disk source (the DAP server uses it for stack traces).
- Insertions are sorted by `(off, pri)`; `pri` orders `{` (0) / hook (1) / `}` (3)
  at the same offset. Braceless bodies (`if (c) x();`) get brace-wrapped, and
  `closeOff` pushes the closing brace past a trailing `;`.
- `Result.Lines` is the set of valid breakpoint lines; `Result.Scopes` is the
  statically-computed name table indexed by `scopeID`.
- Name resolution is **deliberately approximate** (function-granular,
  over-collecting): a wrong name simply fails its guarded eval and is dropped from
  the Variables pane, never displayed wrong. Don't "fix" this by making it precise
  at the cost of complexity.
- `reflectWalk` is the catch-all traversal so unhandled node kinds are still
  descended into. It skips `DeclarationList` and `File` fields — `DeclarationList`
  aliases `Binding` nodes that also appear in statement position, and walking both
  double-instruments initializers.

**`dbg/debugger.go`** — the pause state machine. The hook runs *on the VM
goroutine*; pausing **is** the hook parking in a `select` loop, servicing
`evalCh` requests until `resumeCh` delivers a new mode. The DAP goroutine never
touches the `sobek.Runtime` — that is what makes `-race` clean by construction, so
any new feature must route runtime access through `evalInFrame`. Stepping is
implemented purely as depth comparison against `CaptureCallStack` (`stepHit`), with
no function entry/exit instrumentation. `inEval` suppresses reentrant hooks while
evaluating user expressions. Two lifecycle rules (rationale in
`DESIGN-DECISIONS.md`): teardown goes through `detach` (sink cleared *before*
resuming, mirrored by the hook's under-lock sink re-check before committing to a
pause), and sends to the VM goroutine are always bounded — never add an unbounded
`resumeCh`/`evalCh` send. Blocking host functions must start their work on
another goroutine and park via `HostBlocked(label, done)` on the VM goroutine —
that is what lets a pause request land *during* the call instead of after it
(see DESIGN-DECISIONS.md "Blocked host calls are pause points").

**`dbg/dap.go`** — hand-rolled DAP server (Content-Length-framed JSON, ~15 request
types, one file). `google/go-dap` is the documented escape hatch if the message
surface grows, not the default. All value formatting goes through
`renderTemplate`, a JS IIFE that returns `"<typeof>\x01<display>"` or
`"\x00ERR:<name>"`; the `\x00ERR:` path is how "not accessible here" (TDZ, wrong
frame, over-collected name) is distinguished from a real value.

**`cmd/demo/`** and **`vscode-ext/`** are thin: a host app with a `log()` binding,
and a ~15-line extension mapping debug type `sobek` to a `DebugAdapterServer` TCP
connection.

## Working on this

- The third hook argument (`__x=>eval(__x)`) is load-bearing: direct eval forces
  sobek to keep enclosing scopes materialized, which is what makes reading *and
  writing* locals work. Don't replace it with an indirect eval or a helper call.
- `__dbg` and `__x` are reserved identifiers in debugged scripts.
- Known limitations are enumerated at the end of `DESIGN-DECISIONS.md` (concise
  arrow bodies aren't step targets; variables resolve in the top frame only;
  objects render as flat JSON; `CallScript` sub-scripts share the global scope;
  one VM/thread per debugger instance). These are
  accepted for the prototype — check that list before treating one as a bug.
- Adding a design decision or a new limitation means updating
  `DESIGN-DECISIONS.md` in the same change.
- The README's "porting" section lists the work items for moving this into a real
  project, in priority order (multi-file support first).
