// Event-handler host — the embedding shape most real hosts actually use.
//
// cmd/demo runs a script top to bottom as if it were main. Almost nothing
// real does that: a host loads a script *once* so it can register handlers,
// then calls into JS repeatedly as events arrive. That difference is the
// whole point of this example — the breakpoint you set in a handler doesn't
// fire at startup, it fires on the event that matters, and a conditional
// breakpoint (`event.total > 1000`) is what gets you to event 47 of 200
// without stepping through the other 199.
//
//	events -wait                # hold the event stream until VS Code attaches
//	events -nodebug             # pristine source, no instrumentation
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/grafana/sobek"

	"sobekdbg/dbg"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:4711", "DAP listen address")
	wait := flag.Bool("wait", false, "wait for a debugger before feeding events")
	waitFor := flag.Duration("waittimeout", 30*time.Minute, "how long -wait holds for a debugger")
	nodebug := flag.Bool("nodebug", false, "run without instrumentation (production mode)")
	script := flag.String("script", "examples/events/rules.js", "handler script to load")
	feed := flag.String("events", "examples/events/events.json", "JSON array of events to dispatch")
	flag.Parse()

	vm := sobek.New()
	d := dbg.New()

	// handlers is host state, not script state: the script hands us callables
	// during its registration pass and we hold them for the life of the run.
	handlers := map[string][]sobek.Callable{}

	// on(name, fn) — the registration binding. Deliberately the only way in:
	// the host decides when handlers run, which is what makes the script a
	// participant in the host's lifecycle rather than the top of it.
	err := vm.Set("on", func(call sobek.FunctionCall) sobek.Value {
		name := call.Argument(0).String()
		fn, ok := sobek.AssertFunction(call.Argument(1))
		if !ok {
			panic(vm.NewTypeError("on(%q, fn): second argument must be a function", name))
		}
		handlers[name] = append(handlers[name], fn)
		return sobek.Undefined()
	})
	if err != nil {
		fatal(err)
	}

	err = vm.Set("log", func(call sobek.FunctionCall) sobek.Value {
		parts := make([]string, len(call.Arguments))
		for i, a := range call.Arguments {
			parts[i] = a.String()
		}
		line := strings.Join(parts, " ")
		fmt.Println(line)
		d.Output("stdout", line+"\n")
		return sobek.Undefined()
	})
	if err != nil {
		fatal(err)
	}

	events, err := loadEvents(*feed)
	if err != nil {
		fatal(err)
	}

	// Registration pass: run the script once so its on(...) calls land.
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
			// Registration itself failed; there is nothing to dispatch to.
			d.Done(err)
			fatal(err)
		}
	}

	// Dispatch. Every call below re-enters instrumented code, so breakpoints
	// inside handlers keep hitting — once per matching event, for the whole
	// run. Note there is no Done() in this loop: Done reports *process*
	// completion (it emits terminated + exited), so a host that calls into JS
	// many times calls it exactly once, at the end.
	var failures int
	for i, ev := range events {
		name, _ := ev["type"].(string)
		hs := handlers[name]
		if len(hs) == 0 {
			fmt.Printf("event %d (%s): no handler\n", i, name)
			continue
		}
		arg := vm.ToValue(ev)
		for _, h := range hs {
			// A throwing handler is a script bug, not a host bug: report it
			// and keep feeding. A rules engine that dies on one bad rule is
			// useless, and it's also the more interesting thing to debug.
			if _, err := h(sobek.Undefined(), arg); err != nil {
				failures++
				msg := fmt.Sprintf("event %d (%s): handler failed: %v\n", i, name, err)
				fmt.Fprint(os.Stderr, msg)
				d.Output("stderr", msg)
			}
		}
	}

	summary := fmt.Sprintf("dispatched %d events, %d handler failures\n", len(events), failures)
	fmt.Print(summary)
	d.Output("stdout", summary)

	if !*nodebug {
		d.Done(nil)
		if *wait {
			// Give the client a moment to receive terminated before we exit.
			time.Sleep(200 * time.Millisecond)
		}
	}
	// Deliberately exit 0 even with handler failures: they're reported in the
	// summary, one of them is on purpose (see rules.js), and a demo that ends
	// in `make: *** Error 1` every run reads as broken rather than instructive.
	// A real host would decide this by whether a failed rule is data or a fault.
}

// loadEvents reads a JSON array of objects. Each object's "type" selects the
// handlers; the whole object is passed to them as the event.
func loadEvents(path string) ([]map[string]any, error) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var events []map[string]any
	if err := json.Unmarshal(src, &events); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return events, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "events:", err)
	os.Exit(1)
}
