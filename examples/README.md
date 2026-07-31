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

## `sim/` — pause a live simulation and change it

The script owns the state (a `world` global) and exports `tick()`; the host
owns the clock and the screen, calling `tick()` about eight times a second and
drawing the result as a framed grid. Three entities wander and lose 1 hp per
tick; at 0 hp they stop moving and render lowercase.

```sh
make run-sim           # builds, then holds until VS Code attaches
```

F5, then continue past the entry stop and watch it run. **A** starts on 8 hp,
so it dies within a few seconds — that's your cue.

1. **Hit Pause** (F6) while it's running. The grid freezes because the VM
   goroutine *is* the thing that was paused. You land either on a statement in
   `step()` or in the inter-tick wait ("Paused in host call tick delay") —
   both are real pause points, and both let you evaluate.
2. **Look around.** The Variables pane has the current entity `e` alongside
   the file-scope `world`. In the Debug Console try `e.hp`, `world.tick`,
   `world.entities.length`.
3. **Revive A.** With A showing DEAD, evaluate `world.entities[0].hp = 100`
   and continue. It starts moving again, on screen, immediately. Nothing was
   recompiled and nothing restarted — the debugger wrote into the live object
   graph, which is what the direct-eval thunk buys (DESIGN-DECISIONS.md, "The
   eval thunk").
4. **Conditional breakpoint** on the bounce check (line 53), condition
   `e.name === "C"` — you stop only on C hitting a wall, while B carries on
   bouncing unremarked.

`./bin/sim -nodebug` runs the pristine source; `-ticks` and `-delay` control
length and speed.

### What's different from `events/`

- The host **looks up a function the script defined** (`vm.Get("tick")` +
  `AssertFunction`) instead of being handed callbacks. Both idioms are worth
  seeing; neither needs anything from `dbg`.
- The inter-tick delay goes through **`HostBlocked`** rather than a bare
  `time.Sleep`. That is what makes a Pause pressed between ticks land
  immediately instead of waiting for the next statement to run — and you can
  still evaluate while parked there, because the pause reuses the last
  statement's eval thunk, whose scope is still materialized.
- `log()` is routed to the Debug Console **only**. stdout is the grid; a host
  that owns the screen has to decide where script output goes, and "wherever
  `fmt.Println` points" is the wrong answer.
- The host sets `sobek.TagFieldNameMapper("json", true)` so `ExportTo` reads
  the JS property names. Without it the export silently yields zeros — worth
  knowing, since nothing errors.
