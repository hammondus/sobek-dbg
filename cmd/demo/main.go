// Demo host: runs a JavaScript file in sobek with the DAP debugger attached.
//
//	demo -wait testdata/sample.js     # hold the script until VS Code attaches
//	demo -nodebug testdata/sample.js  # plain sobek, no instrumentation
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grafana/sobek"

	"sobekdbg/dbg"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:4711", "DAP listen address")
	wait := flag.Bool("wait", false, "wait for a debugger before running the script")
	waitFor := flag.Duration("waittimeout", 30*time.Minute, "how long -wait holds for a debugger")
	nodebug := flag.Bool("nodebug", false, "run without instrumentation (production mode)")
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: demo [flags] script.js")
		flag.PrintDefaults()
		os.Exit(2)
	}
	path := flag.Arg(0)

	vm := sobek.New()
	d := dbg.New()

	// A host-provided function, so stepping over calls into Go is exercised.
	err := vm.Set("log", func(call sobek.FunctionCall) sobek.Value {
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

	// A blocking host call routed through HostBlocked, so pausing works while
	// the script sits inside it (try it with testdata/blocking.js).
	err = vm.Set("sleep", func(ms int64) {
		done := make(chan struct{})
		go func() {
			time.Sleep(time.Duration(ms) * time.Millisecond)
			close(done)
		}()
		d.HostBlocked("sleep", done)
	})
	if err != nil {
		fatal(err)
	}

	// Script-calls-script: run another file from the same directory in the
	// same VM (globals are shared — see DESIGN-DECISIONS.md). A sub-script
	// failure surfaces as a catchable exception in the caller.
	err = vm.Set("call", func(name string) sobek.Value {
		sub := filepath.Join(filepath.Dir(path), name)
		var v sobek.Value
		var callErr error
		if *nodebug {
			var src []byte
			if src, callErr = os.ReadFile(sub); callErr == nil {
				v, callErr = vm.RunScript(sub, string(src))
			}
		} else {
			v, callErr = d.CallScript(sub)
		}
		if callErr != nil {
			panic(vm.NewGoError(callErr))
		}
		return v
	})
	if err != nil {
		fatal(err)
	}

	if *nodebug {
		src, err := os.ReadFile(path)
		if err != nil {
			fatal(err)
		}
		if _, err := vm.RunScript(path, string(src)); err != nil {
			fatal(err)
		}
		return
	}

	prog, err := d.Load(path)
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
	fmt.Printf("DAP server on %s — attach from VS Code (launch config \"Attach to sobek\")\n", *addr)

	if *wait {
		fmt.Println("waiting for debugger…")
		if !d.WaitConfigured(*waitFor) {
			fatal(fmt.Errorf("no debugger attached within %v", *waitFor))
		}
	}

	_, err = vm.RunProgram(prog)
	d.Done(err)
	if err != nil {
		fatal(err)
	}
	if *wait {
		// Give the client a moment to receive terminated before we exit.
		time.Sleep(200 * time.Millisecond)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "demo:", err)
	os.Exit(1)
}
