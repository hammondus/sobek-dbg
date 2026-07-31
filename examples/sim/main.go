// Tick-loop host — a live simulation you can pause, inspect and *change*.
//
// The script owns the state (a `world` global) and exports a `tick()` the
// host calls at a fixed rate; the host owns the clock and the screen. That
// split is what makes the debugger's write-to-locals support visible rather
// than theoretical: pause mid-run, set an entity's hp in the Debug Console,
// continue, and watch it start moving again on the grid.
//
// Two things here that the events example doesn't cover: the host looks up a
// named function the script defined (rather than being handed callbacks), and
// the inter-tick delay goes through HostBlocked, so a Pause pressed between
// ticks lands immediately instead of waiting for the next statement.
//
//	sim -wait          # hold the simulation until VS Code attaches
//	sim -nodebug       # pristine source, no instrumentation
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/grafana/sobek"

	"sobekdbg/dbg"
)

// The shape the host expects the script's `world` global to have. sobek's
// ExportTo maps it by json tag, so the JS names are the contract.
type world struct {
	Tick     int      `json:"tick"`
	Width    int      `json:"width"`
	Height   int      `json:"height"`
	Entities []entity `json:"entities"`
}

type entity struct {
	Name string `json:"name"`
	X    int    `json:"x"`
	Y    int    `json:"y"`
	HP   int    `json:"hp"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:4711", "DAP listen address")
	wait := flag.Bool("wait", false, "wait for a debugger before starting the clock")
	waitFor := flag.Duration("waittimeout", 30*time.Minute, "how long -wait holds for a debugger")
	nodebug := flag.Bool("nodebug", false, "run without instrumentation (production mode)")
	script := flag.String("script", "examples/sim/sim.js", "simulation script")
	ticks := flag.Int("ticks", 300, "how many ticks to run")
	delay := flag.Duration("delay", 120*time.Millisecond, "wall-clock time per tick")
	flag.Parse()

	vm := sobek.New()
	// Without a mapper, ExportTo matches exact Go field names ("Tick"), so a
	// world of lowercase JS properties silently exports as zeros. Making the
	// json tags authoritative is what lets the JS property names be the
	// host/script contract they're documented as.
	vm.SetFieldNameMapper(sobek.TagFieldNameMapper("json", true))
	d := dbg.New()

	// log() goes to the Debug Console *only*. stdout is the grid: printing
	// script output there would tear the frame. A host that owns the screen
	// has to decide where script output goes, and "wherever fmt.Println
	// happens to point" is the wrong answer.
	err := vm.Set("log", func(call sobek.FunctionCall) sobek.Value {
		parts := make([]string, len(call.Arguments))
		for i, a := range call.Arguments {
			parts[i] = a.String()
		}
		d.Output("stdout", strings.Join(parts, " ")+"\n")
		return sobek.Undefined()
	})
	if err != nil {
		fatal(err)
	}

	// Definition pass: run the script so `world` and `tick` exist.
	if *nodebug {
		src, err := os.ReadFile(*script)
		if err != nil {
			fatal(err)
		}
		if _, err := vm.RunScript(*script, string(src)); err != nil {
			fatal(err)
		}
	} else {
		prog, err := d.Load(*script)
		if err != nil {
			fatal(err)
		}
		if err := d.Attach(vm); err != nil {
			fatal(err)
		}
		srv := &dbg.Server{D: d}
		go func() {
			if err := srv.ListenAndServe(*addr); err != nil {
				fatal(err)
			}
		}()
		fmt.Printf("DAP server on %s — attach from VS Code (\"Attach to sobek demo\")\n", *addr)
		if *wait {
			fmt.Println("waiting for debugger…")
			if !d.WaitConfigured(*waitFor) {
				fatal(fmt.Errorf("no debugger attached within %v", *waitFor))
			}
		}
		if _, err := vm.RunProgram(prog); err != nil {
			d.Done(err)
			fatal(err)
		}
	}

	// The host/script contract: a global named tick, taking no arguments.
	tick, ok := sobek.AssertFunction(vm.Get("tick"))
	if !ok {
		err := fmt.Errorf("%s must define a global function tick()", *script)
		d.Done(err)
		fatal(err)
	}

	for i := 0; i < *ticks; i++ {
		if _, err := tick(sobek.Undefined()); err != nil {
			d.Done(err)
			fatal(err)
		}

		var w world
		if err := vm.ExportTo(vm.Get("world"), &w); err != nil {
			d.Done(err)
			fatal(err)
		}
		fmt.Print(render(w))

		// Through HostBlocked rather than a plain Sleep: this is the VM
		// goroutine, and parking here makes the wait a pause point. Press
		// Pause between ticks and it lands now, on the statement that ran
		// last, with its scope still live — not at the next statement.
		done := make(chan struct{})
		go func() {
			time.Sleep(*delay)
			close(done)
		}()
		d.HostBlocked("tick delay", done)
	}

	if !*nodebug {
		d.Done(nil)
		time.Sleep(200 * time.Millisecond)
	}
}

// render draws the world as a framed grid plus a status line, redrawing over
// the previous frame. Returns the whole frame as one string so it reaches the
// terminal in a single write.
func render(w world) string {
	if w.Width <= 0 || w.Height <= 0 {
		return "world has no extent — check width/height in the script\n"
	}
	grid := make([][]byte, w.Height)
	for y := range grid {
		grid[y] = bytes.Repeat([]byte{' '}, w.Width)
	}
	for _, e := range w.Entities {
		if e.X < 0 || e.X >= w.Width || e.Y < 0 || e.Y >= w.Height {
			continue // the script put it off-grid; that's its business
		}
		name := "?"
		if e.Name != "" {
			name = e.Name[:1]
		}
		if e.HP <= 0 {
			name = strings.ToLower(name) // dead: lowercase, and it stops moving
		}
		grid[e.Y][e.X] = name[0]
	}

	var b strings.Builder
	edge := strings.Repeat("─", w.Width)
	b.WriteString("\033[H\033[2J") // home, clear
	fmt.Fprintf(&b, "┌%s┐\n", edge)
	for _, row := range grid {
		fmt.Fprintf(&b, "│%s│\n", row)
	}
	fmt.Fprintf(&b, "└%s┘\n", edge)

	fmt.Fprintf(&b, "tick %-5d", w.Tick)
	for _, e := range w.Entities {
		state := ""
		if e.HP <= 0 {
			state = " DEAD"
		}
		fmt.Fprintf(&b, "  %s hp=%-4d(%2d,%2d)%s", e.Name, e.HP, e.X, e.Y, state)
	}
	b.WriteString("\n")
	return b.String()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "sim:", err)
	os.Exit(1)
}
