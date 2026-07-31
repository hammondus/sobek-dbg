# Example hosts

Separate Go module (`sobekdbg/examples`, with a `replace` pointing at `../`)
so a demo can never drag a dependency into the debugger's `go.mod`, and
`go get sobekdbg` never sees any of this. Same clone, same `make` — see
DESIGN-DECISIONS.md, "Repo layout".

Build and run from the repo root; `go build ./...` at the root will *not*
reach in here, which is the point.

## `events/` — script as event handler

The shape most real embeddings take, and the one `cmd/demo` doesn't show:
the host loads `rules.js` **once** so its `on(...)` calls register handlers,
then calls into JS repeatedly as events arrive from `events.json`.

```sh
make run-events        # builds, then holds until VS Code attaches
```

Then F5 ("Attach to sobek demo"). `stopOnEntry` lands you on the first line
of the registration pass — continue past it, since the interesting code runs
later.

Things worth trying, in order:

1. **Breakpoint on line 17** (`const value = event.total * event.items`).
   It doesn't fire at startup — it fires once per `order.created`, five
   times over the run. That's the difference from a script-as-main host.
2. **Make it conditional: `event.total > 1000`.** Now it stops only on
   A-1004, with `ordersSeen` and `revenue` already carrying the state of
   every event before it. Finding one bad event in a stream without stepping
   through the rest is most of why this debugger exists.
3. **Inspect and mutate.** Paused in a handler, the Variables pane shows the
   handler's locals *and* the file-scope totals. In the Debug Console:
   `revenue`, `event.items * 2`, then `revenue = 0` — the assignment sticks
   and the final summary changes.
4. **Breakpoint on line 42**, in the `order.refunded` handler. One refund in
   the feed has no `reason`, so `event.reason.trim()` throws. The host
   reports the failure and keeps feeding, so you arrive at the broken event
   with every earlier one already applied.

Run the production path with `./bin/events -nodebug` — pristine source, no
hooks, byte-identical output.

### What the host actually does

Nine calls, all of them in `main.go`:

- `dbg.New()`, then `Load(script)` and `Attach(vm)` before running anything
- `Server{D: d}.ListenAndServe(addr)` on its own goroutine
- `WaitConfigured(timeout)` to hold the stream until breakpoints are set
- `RunProgram` for the registration pass, then plain `sobek.Callable` calls
  per event — instrumented code either way, so breakpoints keep hitting
- `Output` to mirror `log()` into the Debug Console
- `Done(nil)` **once**, at the end. It reports process completion (it emits
  `terminated` and `exited`), so it does not belong in the dispatch loop.

The VM is only ever touched from `main`'s goroutine. That is the one rule a
host has to keep.
