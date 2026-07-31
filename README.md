# sobekdbg — VS Code step debugging for sobek, without forking the VM

Proof of concept: full DAP debugging (breakpoints, conditional breakpoints,
step in/over/out, call stack, variable inspection, debug-console evaluation
*including assignment to locals*) for JavaScript running in an **unmodified
upstream sobek**, by instrumenting the script source in debug mode.

See `DESIGN-DECISIONS.md` for how and why. Layout:

- `instrument/` — parses with sobek's own parser, splices a hook before every
  statement (line numbers preserved), computes visible variable names.
- `dbg/` — the debugger (pause/step/breakpoint state machine on the VM
  goroutine) and a hand-rolled DAP server. sobek is the only dependency.
- `cmd/demo/` — a host app running `testdata/sample.js` with a `log()` Go
  binding.
- `vscode-ext/` — ~15-line extension registering the `sobek` debug type.

## Try it

```sh
make install-ext   # once; then reload VS Code
make run           # starts DAP on 127.0.0.1:4711, waits for the debugger
```

In VS Code: open this folder, set a breakpoint in `testdata/sample.js`,
press F5 ("Attach to sobek demo"). `stopOnEntry` is on in the launch config,
so it pauses on the first line even with no breakpoints.

While paused, try in the Debug Console: `counter`, `item.value * 10`,
`counter = 1000` — the assignment sticks.

Run without debugging (what production would do): `./bin/demo -nodebug testdata/sample.js`.

## Examples

`examples/` is a separate module (so demos can't leak dependencies into the
debugger's `go.mod`) holding example hosts — both showing embedding shapes
`cmd/demo` doesn't, where the host calls *into* JS rather than running a
script as main:

```sh
make run-events    # script registers handlers, host feeds it events
make run-sim       # live tick loop: pause it, revive a dead entity from
                   # the Debug Console, watch the grid change
```

Then F5. See `examples/README.md` for what to try in each.

## Tests

`make test` — includes an end-to-end test that speaks DAP over TCP and
asserts stepping, stack, variables, conditional breakpoints, and that a
debugger-side mutation changes the script's result.


# porting
When you port this into the real project, the work items in rough priority order:

1. Concurrent script runs as DAP threads — dbg is multi-file now (breakpoints and scope tables keyed by path, script-calls-script stepping works), but still assumes one VM running one script at a time; background jobs need a run registry presenting each running script as a DAP thread.
2. Expandable objects in the Variables pane — currently flat JSON strings; the upgrade is handing out variablesReference IDs that map to property-path evals. Cosmetic but you'll want it.
3. A .d.ts for your host API — unrelated to the debugger but completes the "VS Code understands it" story with IntelliSense for script authors.
4. `localRoot`/`remoteRoot` path mapping — needed the moment the editor and the host aren't on the same machine (scripts authored on a laptop, executed on the server next to the database). Path matching today goes through `EvalSymlinks` + case-folding, which assumes a shared filesystem. Apply the mapping in both directions: local→remote on `setBreakpoints`, remote→local on stack frames and `source`. Same mechanism delve and node use.

Two things to keep in mind from DESIGN-DECISIONS.md: concise arrows (x => x*2) aren't step targets (write braces), and variables only resolve in the top stack frame. Both livable, both have known fixes if they ever grate.