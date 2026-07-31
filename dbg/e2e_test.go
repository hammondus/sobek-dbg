package dbg

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grafana/sobek"
)

// Line numbers matter: tests reference them.
const sampleJS = `var total = 0;
function addOne(n) {
	var doubled = n * 2;
	total = total + doubled;
	return total;
}
for (let i = 1; i <= 3; i++) {
	addOne(i);
}
var obj = { name: "t", vals: [1, 2] };
total = total + obj.vals.length;
result(total);
`

// startTarget spins up a debuggee: instrumented script + DAP server on an
// ephemeral port. The script starts once the client sends configurationDone.
func startTarget(t *testing.T) (addr, path string, result chan int64) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "sample.js")
	if err := os.WriteFile(path, []byte(sampleJS), 0o644); err != nil {
		t.Fatal(err)
	}

	d := New()
	prog, err := d.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	vm := sobek.New()
	result = make(chan int64, 1)
	if err := vm.Set("result", func(v int64) { result <- v }); err != nil {
		t.Fatal(err)
	}
	if err := d.Attach(vm); err != nil {
		t.Fatal(err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	srv := &Server{D: d}
	go srv.Serve(l)

	go func() {
		if !d.WaitConfigured(10 * time.Second) {
			t.Error("debugger never configured")
			return
		}
		_, err := vm.RunProgram(prog)
		d.Done(err)
		if err != nil {
			t.Error(err)
		}
	}()
	return l.Addr().String(), path, result
}

type cli struct {
	t       *testing.T
	conn    net.Conn
	r       *bufio.Reader
	seq     int
	pending []map[string]any // events received while waiting for a response
}

func dial(t *testing.T, addr string) *cli {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetReadDeadline(time.Now().Add(15 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return &cli{t: t, conn: conn, r: bufio.NewReader(conn)}
}

func (c *cli) read() map[string]any {
	c.t.Helper()
	var length int
	for {
		line, err := c.r.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		fmt.Sscanf(line, "Content-Length: %d", &length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		c.t.Fatalf("read body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf, &m); err != nil {
		c.t.Fatalf("bad JSON %q: %v", buf, err)
	}
	return m
}

// req sends a request and returns its (successful) response body, buffering
// any events that arrive first.
func (c *cli) req(cmd string, args any) map[string]any {
	c.t.Helper()
	c.seq++
	msg, _ := json.Marshal(map[string]any{
		"seq": c.seq, "type": "request", "command": cmd, "arguments": args,
	})
	fmt.Fprintf(c.conn, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
	for {
		m := c.read()
		if m["type"] == "event" {
			c.pending = append(c.pending, m)
			continue
		}
		if m["command"] != cmd {
			c.t.Fatalf("response for %q while waiting for %q", m["command"], cmd)
		}
		if m["success"] != true {
			c.t.Fatalf("%s failed: %v", cmd, m["message"])
		}
		body, _ := m["body"].(map[string]any)
		return body
	}
}

func (c *cli) reqFail(cmd string, args any) string {
	c.t.Helper()
	c.seq++
	msg, _ := json.Marshal(map[string]any{
		"seq": c.seq, "type": "request", "command": cmd, "arguments": args,
	})
	fmt.Fprintf(c.conn, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
	for {
		m := c.read()
		if m["type"] == "event" {
			c.pending = append(c.pending, m)
			continue
		}
		if m["success"] == true {
			c.t.Fatalf("%s unexpectedly succeeded", cmd)
		}
		s, _ := m["message"].(string)
		return s
	}
}

func (c *cli) waitEvent(name string) map[string]any {
	c.t.Helper()
	for i, m := range c.pending {
		if m["event"] == name {
			c.pending = append(c.pending[:i], c.pending[i+1:]...)
			return m
		}
	}
	for {
		m := c.read()
		if m["type"] == "event" {
			if m["event"] == name {
				return m
			}
			c.pending = append(c.pending, m)
			continue
		}
		c.t.Fatalf("unexpected response %v while waiting for event %q", m["command"], name)
	}
}

func body(m map[string]any) map[string]any {
	b, _ := m["body"].(map[string]any)
	return b
}

func bpArgs(path string, bps ...map[string]any) map[string]any {
	return map[string]any{
		"source":      map[string]any{"path": path},
		"breakpoints": bps,
	}
}

func (c *cli) handshake(path string, bps ...map[string]any) {
	c.t.Helper()
	c.req("initialize", map[string]any{"adapterID": "sobek"})
	c.waitEvent("initialized")
	c.req("attach", map[string]any{})
	res := c.req("setBreakpoints", bpArgs(path, bps...))
	for _, b := range res["breakpoints"].([]any) {
		if b.(map[string]any)["verified"] != true {
			c.t.Fatalf("breakpoint not verified: %v", b)
		}
	}
	c.req("configurationDone", nil)
}

func TestBreakpointStackVariablesEvalStep(t *testing.T) {
	addr, path, result := startTarget(t)
	c := dial(t, addr)
	c.handshake(path, map[string]any{"line": 4})

	// --- stopped at the breakpoint ---
	ev := body(c.waitEvent("stopped"))
	if ev["reason"] != "breakpoint" {
		t.Fatalf("stop reason = %v, want breakpoint", ev["reason"])
	}

	st := c.req("stackTrace", map[string]any{"threadId": 1})
	frames := st["stackFrames"].([]any)
	if len(frames) < 2 {
		t.Fatalf("want >=2 frames, got %v", frames)
	}
	top := frames[0].(map[string]any)
	if top["name"] != "addOne" || top["line"] != float64(4) {
		t.Errorf("top frame = %v, want addOne line 4", top)
	}
	if p := top["source"].(map[string]any)["path"]; p != path {
		t.Errorf("frame path = %v, want %v", p, path)
	}
	// Column must refer to the on-disk source (statement starts after the
	// tab, column 2), not the instrumented text with the hook spliced in.
	if c := top["column"]; c != float64(2) {
		t.Errorf("top frame column = %v, want 2 (instrumented-source skew?)", c)
	}

	// --- variables in the paused frame ---
	c.req("scopes", map[string]any{"frameId": 0})
	vars := map[string]string{}
	for _, v := range c.req("variables", map[string]any{"variablesReference": 1})["variables"].([]any) {
		vv := v.(map[string]any)
		vars[vv["name"].(string)] = vv["value"].(string)
	}
	if vars["n"] != "1" || vars["doubled"] != "2" || vars["total"] != "0" {
		t.Errorf("variables = %v, want n=1 doubled=2 total=0", vars)
	}

	// --- evaluate, including debugger-driven mutation ---
	if r := c.req("evaluate", map[string]any{"expression": "n + doubled"}); r["result"] != "3" {
		t.Errorf("evaluate n+doubled = %v, want 3", r["result"])
	}
	c.req("evaluate", map[string]any{"expression": "total = 90"})
	// Variables-pane editing (setVariable) must also stick: 90 -> 100.
	if r := c.req("setVariable", map[string]any{"variablesReference": 1, "name": "total", "value": "100"}); r["value"] != "100" {
		t.Errorf("setVariable returned %v, want 100", r["value"])
	}
	if msg := c.reqFail("evaluate", map[string]any{"expression": "nosuchvar"}); !strings.Contains(msg, "ReferenceError") {
		t.Errorf("expected ReferenceError, got %q", msg)
	}
	// A trailing line comment must not swallow the eval template's close paren.
	if r := c.req("evaluate", map[string]any{"expression": "n + doubled // sum"}); r["result"] != "3" {
		t.Errorf("evaluate with trailing comment = %v, want 3", r["result"])
	}
	// setVariable names come back from the client verbatim; only identifiers
	// may be spliced into the assignment.
	if msg := c.reqFail("setVariable", map[string]any{"variablesReference": 1, "name": "n; total = 0", "value": "1"}); msg != "not a variable" {
		t.Errorf("setVariable with non-identifier: %q, want rejection", msg)
	}

	// --- step over: line 4 -> line 5 ---
	c.req("setBreakpoints", bpArgs(path)) // clear, or the loop re-triggers
	c.req("next", map[string]any{"threadId": 1})
	if ev := body(c.waitEvent("stopped")); ev["reason"] != "step" {
		t.Fatalf("stop reason = %v, want step", ev["reason"])
	}
	st = c.req("stackTrace", map[string]any{"threadId": 1})
	if l := st["stackFrames"].([]any)[0].(map[string]any)["line"]; l != float64(5) {
		t.Errorf("after next: line = %v, want 5", l)
	}

	// --- step out: back into top-level code ---
	c.req("stepOut", map[string]any{"threadId": 1})
	c.waitEvent("stopped")
	st = c.req("stackTrace", map[string]any{"threadId": 1})
	if n := len(st["stackFrames"].([]any)); n != 1 {
		t.Errorf("after stepOut: %d frames, want 1", n)
	}

	// --- run to completion; the mutation must be visible in the result ---
	c.req("continue", map[string]any{"threadId": 1})
	c.waitEvent("terminated")
	// total: 100 (mutated) +2 already added, then +4, +6, +2 = 114
	select {
	case r := <-result:
		if r != 114 {
			t.Errorf("result = %d, want 114 (debugger mutation lost?)", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("script did not finish")
	}
}

func TestConditionalBreakpoint(t *testing.T) {
	addr, path, result := startTarget(t)
	c := dial(t, addr)
	// The trailing line comment exercises the close-paren guard in conditionHit.
	c.handshake(path, map[string]any{"line": 8, "condition": "i === 2 // middle pass only"})

	body(c.waitEvent("stopped"))
	if r := c.req("evaluate", map[string]any{"expression": "i"}); r["result"] != "2" {
		t.Errorf("i = %v, want 2 (condition ignored?)", r["result"])
	}
	c.req("continue", map[string]any{"threadId": 1})
	c.waitEvent("terminated") // i===3 must not re-trigger
	if r := <-result; r != 14 {
		t.Errorf("result = %d, want 14", r)
	}
}

func TestStepIntoFunction(t *testing.T) {
	addr, path, result := startTarget(t)
	c := dial(t, addr)
	c.handshake(path, map[string]any{"line": 8})

	c.waitEvent("stopped")
	c.req("setBreakpoints", bpArgs(path))
	c.req("stepIn", map[string]any{"threadId": 1})
	c.waitEvent("stopped")
	st := c.req("stackTrace", map[string]any{"threadId": 1})
	top := st["stackFrames"].([]any)[0].(map[string]any)
	if top["name"] != "addOne" || top["line"] != float64(3) {
		t.Errorf("after stepIn: %v, want addOne line 3", top)
	}
	c.req("continue", map[string]any{"threadId": 1})
	c.waitEvent("terminated")
	<-result
}

// A script that fails must be reported as a failed run, not exit code 0.
func TestScriptErrorExitCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "boom.js")
	if err := os.WriteFile(path, []byte("var x = 1;\nthrow new Error(\"boom\");\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New()
	prog, err := d.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	vm := sobek.New()
	if err := d.Attach(vm); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go (&Server{D: d}).Serve(l)
	go func() {
		d.WaitConfigured(10 * time.Second)
		_, err := vm.RunProgram(prog)
		d.Done(err)
	}()

	c := dial(t, l.Addr().String())
	c.handshake(path)
	c.waitEvent("terminated")
	if ev := body(c.waitEvent("exited")); ev["exitCode"] != float64(1) {
		t.Errorf("exitCode = %v, want 1 for a script error", ev["exitCode"])
	}
}

// The editor may know the script by a path that reaches the same file through
// a symlink (macOS /tmp -> /private/tmp, symlinked workspaces). Breakpoints
// must still match.
func TestSamePathThroughSymlinks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.js")
	if err := os.WriteFile(p, []byte("1;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if !samePath(p, filepath.Join(link, "s.js")) {
		t.Errorf("samePath(%q, %q) = false, want true", p, filepath.Join(link, "s.js"))
	}
	if samePath(p, filepath.Join(dir, "other.js")) {
		t.Error("samePath matched two different files")
	}
}

// A new attach must take over from a stale session (which may be a half-dead
// socket) instead of queueing unanswered behind it in the listen backlog.
func TestNewAttachKicksStaleSession(t *testing.T) {
	addr, path, result := startTarget(t)
	c1 := dial(t, addr)
	c1.handshake(path, map[string]any{"line": 4})
	c1.waitEvent("stopped")

	// c1 is neither disconnected nor reading; a second client attaches.
	c2 := dial(t, addr)
	c2.handshake(path) // no breakpoints: clears c1's
	c2.req("continue", map[string]any{"threadId": 1})
	c2.waitEvent("terminated")
	select {
	case r := <-result:
		if r != 14 {
			t.Errorf("result = %d, want 14", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("script did not finish after takeover")
	}
}

// A pause request arriving while the VM goroutine is blocked inside a host
// call must stop the script at the calling statement — with working
// evaluation and mutation — instead of being deferred until the call
// returns. Continue must send it back to waiting, and a second pause must
// still work.
func TestPauseDuringBlockedHostCall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "block.js")
	src := "var x = 1;\nblock();\nx = x + 1;\ndone(x);\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New()
	prog, err := d.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	vm := sobek.New()
	entered := make(chan struct{})
	release := make(chan struct{})
	result := make(chan int64, 1)
	vm.Set("done", func(v int64) { result <- v })
	vm.Set("block", func() {
		close(entered)
		d.HostBlocked("test.block", release)
	})
	d.Attach(vm)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go (&Server{D: d}).Serve(l)
	go func() {
		d.WaitConfigured(10 * time.Second)
		_, err := vm.RunProgram(prog)
		d.Done(err)
	}()

	c := dial(t, l.Addr().String())
	c.handshake(path)
	// The script is inside block(). The pause below may still race
	// HostBlocked's registration; its pending-pauseReq check covers that.
	<-entered

	c.req("pause", map[string]any{"threadId": 1})
	if ev := body(c.waitEvent("stopped")); ev["reason"] != "pause" {
		t.Fatalf("reason = %v, want pause", ev["reason"])
	}
	st := c.req("stackTrace", map[string]any{"threadId": 1})
	if l := st["stackFrames"].([]any)[0].(map[string]any)["line"]; l != float64(2) {
		t.Errorf("paused line = %v, want 2 (the block() call)", l)
	}
	if r := c.req("evaluate", map[string]any{"expression": "x"}); r["result"] != "1" {
		t.Errorf("x = %v, want 1", r["result"])
	}
	// Mutation while paused inside the host call must stick.
	c.req("evaluate", map[string]any{"expression": "x = 41"})

	// Continue returns the script to its wait; a second pause must land too.
	c.req("continue", map[string]any{"threadId": 1})
	c.req("pause", map[string]any{"threadId": 1})
	if ev := body(c.waitEvent("stopped")); ev["reason"] != "pause" {
		t.Fatalf("second pause reason = %v, want pause", ev["reason"])
	}
	c.req("continue", map[string]any{"threadId": 1})

	close(release)
	c.waitEvent("terminated")
	select {
	case r := <-result:
		if r != 42 {
			t.Errorf("result = %d, want 42 (mutation during blocked pause lost?)", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("script did not finish")
	}
}

// startCallTarget spins up a two-script debuggee: main.js runs sub.js via a
// call() binding built on CallScript. Line numbers matter — tests use them.
//
//	main.js 1: var v = 1;          sub.js 1: var subAdded = 10;
//	        2: call("sub.js");             2: subAdded = subAdded + 1;
//	        3: v = v + subAdded;
//	        4: done(v);
func startCallTarget(t *testing.T) (addr, mainPath, subPath string, result chan int64) {
	t.Helper()
	dir := t.TempDir()
	mainPath = filepath.Join(dir, "main.js")
	subPath = filepath.Join(dir, "sub.js")
	mainJS := "var v = 1;\ncall(\"sub.js\");\nv = v + subAdded;\ndone(v);\n"
	subJS := "var subAdded = 10;\nsubAdded = subAdded + 1;\n"
	if err := os.WriteFile(mainPath, []byte(mainJS), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(subPath, []byte(subJS), 0o644); err != nil {
		t.Fatal(err)
	}

	d := New()
	prog, err := d.Load(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	vm := sobek.New()
	result = make(chan int64, 1)
	vm.Set("done", func(v int64) { result <- v })
	vm.Set("call", func(name string) sobek.Value {
		v, err := d.CallScript(filepath.Join(dir, name))
		if err != nil {
			panic(vm.NewGoError(err))
		}
		return v
	})
	if err := d.Attach(vm); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go (&Server{D: d}).Serve(l)
	go func() {
		d.WaitConfigured(10 * time.Second)
		_, err := vm.RunProgram(prog)
		d.Done(err)
		if err != nil {
			t.Error(err)
		}
	}()
	return l.Addr().String(), mainPath, subPath, result
}

func (c *cli) topFrame() (path string, line float64) {
	c.t.Helper()
	st := c.req("stackTrace", map[string]any{"threadId": 1})
	top := st["stackFrames"].([]any)[0].(map[string]any)
	return top["source"].(map[string]any)["path"].(string), top["line"].(float64)
}

// Step-over on a call-script statement must free-run the sub-script and stop
// at the caller's next statement, with the sub-script's effects visible.
func TestCallScriptStepOver(t *testing.T) {
	addr, mainPath, _, result := startCallTarget(t)
	c := dial(t, addr)
	c.handshake(mainPath, map[string]any{"line": 2})
	c.waitEvent("stopped")
	c.req("setBreakpoints", bpArgs(mainPath)) // clear

	c.req("next", map[string]any{"threadId": 1})
	if ev := body(c.waitEvent("stopped")); ev["reason"] != "step" {
		t.Fatalf("reason = %v, want step", ev["reason"])
	}
	if p, l := c.topFrame(); p != mainPath || l != 3 {
		t.Errorf("after step over call(): at %s:%v, want %s:3", p, l, mainPath)
	}
	// The sub-script ran to completion during the step.
	if r := c.req("evaluate", map[string]any{"expression": "subAdded"}); r["result"] != "11" {
		t.Errorf("subAdded = %v, want 11", r["result"])
	}
	c.req("continue", map[string]any{"threadId": 1})
	c.waitEvent("terminated")
	if r := <-result; r != 12 {
		t.Errorf("result = %d, want 12", r)
	}
}

// Step-in on a call-script statement descends to the sub-script's first
// statement; step-out from inside the sub-script returns to the caller.
func TestCallScriptStepInAndOut(t *testing.T) {
	addr, mainPath, subPath, result := startCallTarget(t)
	c := dial(t, addr)
	c.handshake(mainPath, map[string]any{"line": 2})
	c.waitEvent("stopped")
	c.req("setBreakpoints", bpArgs(mainPath)) // clear

	c.req("stepIn", map[string]any{"threadId": 1})
	c.waitEvent("stopped")
	if p, l := c.topFrame(); p != subPath || l != 1 {
		t.Fatalf("after step in: at %s:%v, want %s:1", p, l, subPath)
	}
	// The caller's frame is below; globals are shared across the boundary.
	st := c.req("stackTrace", map[string]any{"threadId": 1})
	frames := st["stackFrames"].([]any)
	if len(frames) != 2 {
		t.Fatalf("want 2 frames (sub + main), got %d: %v", len(frames), frames)
	}
	parent := frames[1].(map[string]any)
	if p := parent["source"].(map[string]any)["path"]; p != mainPath || parent["line"] != float64(2) {
		t.Errorf("parent frame = %v, want %s:2", parent, mainPath)
	}
	if r := c.req("evaluate", map[string]any{"expression": "v"}); r["result"] != "1" {
		t.Errorf("v = %v, want 1", r["result"])
	}

	c.req("stepOut", map[string]any{"threadId": 1})
	c.waitEvent("stopped")
	if p, l := c.topFrame(); p != mainPath || l != 3 {
		t.Errorf("after step out: at %s:%v, want %s:3", p, l, mainPath)
	}
	c.req("continue", map[string]any{"threadId": 1})
	c.waitEvent("terminated")
	if r := <-result; r != 12 {
		t.Errorf("result = %d, want 12", r)
	}
}

// Breakpoints set in a script that hasn't been loaded yet must verify (the
// debugger loads it eagerly) and hit when a call-script run reaches them.
func TestBreakpointInSubScript(t *testing.T) {
	addr, _, subPath, result := startCallTarget(t)
	c := dial(t, addr)
	c.req("initialize", map[string]any{"adapterID": "sobek"})
	c.waitEvent("initialized")
	c.req("attach", map[string]any{})
	res := c.req("setBreakpoints", bpArgs(subPath, map[string]any{"line": 2}))
	for _, b := range res["breakpoints"].([]any) {
		if b.(map[string]any)["verified"] != true {
			t.Fatalf("sub-script breakpoint not verified (eager load broken?): %v", b)
		}
	}
	c.req("configurationDone", nil)

	if ev := body(c.waitEvent("stopped")); ev["reason"] != "breakpoint" {
		t.Fatalf("reason = %v, want breakpoint", ev["reason"])
	}
	if p, l := c.topFrame(); p != subPath || l != 2 {
		t.Errorf("stopped at %s:%v, want %s:2", p, l, subPath)
	}
	if r := c.req("evaluate", map[string]any{"expression": "subAdded"}); r["result"] != "10" {
		t.Errorf("subAdded = %v, want 10", r["result"])
	}
	c.req("continue", map[string]any{"threadId": 1})
	c.waitEvent("terminated")
	if r := <-result; r != 12 {
		t.Errorf("result = %d, want 12", r)
	}
}

// Hosts call HostBlocked unconditionally; with no debugger wired it must
// degrade to a plain wait, not panic.
func TestHostBlockedNilDebugger(t *testing.T) {
	done := make(chan struct{})
	close(done)
	(*Debugger)(nil).HostBlocked("x", done)
}

func TestDebuggerStatementAndVariablesObject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dbgstmt.js")
	src := "var o = { a: 1, b: [1, 2] };\nvar s = \"hi\";\ndebugger;\ndone(o.a);\n"
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	d := New()
	prog, err := d.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	vm := sobek.New()
	done := make(chan int64, 1)
	vm.Set("done", func(v int64) { done <- v })
	d.Attach(vm)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go (&Server{D: d}).Serve(l)
	go func() {
		d.WaitConfigured(10 * time.Second)
		_, err := vm.RunProgram(prog)
		d.Done(err)
	}()

	c := dial(t, l.Addr().String())
	c.handshake(path) // no breakpoints: the debugger statement alone must stop it

	ev := body(c.waitEvent("stopped"))
	if ev["reason"] != "debugger statement" {
		t.Fatalf("reason = %v", ev["reason"])
	}
	vars := map[string]map[string]any{}
	for _, v := range c.req("variables", map[string]any{"variablesReference": 1})["variables"].([]any) {
		vv := v.(map[string]any)
		vars[vv["name"].(string)] = vv
	}
	if vars["o"]["value"] != `{"a":1,"b":[1,2]}` || vars["o"]["type"] != "object" {
		t.Errorf("o = %v", vars["o"])
	}
	if vars["s"]["value"] != `"hi"` || vars["s"]["type"] != "string" {
		t.Errorf("s = %v", vars["s"])
	}
	c.req("continue", map[string]any{"threadId": 1})
	c.waitEvent("terminated")
	<-done
}
