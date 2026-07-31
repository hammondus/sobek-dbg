package instrument

import (
	"strings"
	"testing"

	"github.com/grafana/sobek"
)

// gnarly exercises the rewrite edge cases: directive prologue, braceless
// if/else/loop bodies, else-if chains, labelled continue, switch fallthrough,
// destructuring, classes, generators, arrows (block and concise bodies),
// try/catch, ASI-reliant statements.
const gnarly = `"use strict";
var out = [];
function log(x) { out.push(x) }
var a = 1
var b = 2
if (a < b) log("lt"); else log("ge")
if (a > b)
	log("never")
else if (a === 1)
	log("one")
for (let i = 0; i < 3; i++) log(i)
outer:
for (let i = 0; i < 3; i++) {
	for (let j = 0; j < 3; j++) {
		if (j > i) continue outer
		log(i * 10 + j)
	}
}
let k = 0
while (k < 2) k++
do k--; while (k > 0)
switch (a) {
case 0:
	log("zero")
case 1:
	log("one-fall")
case 2:
	log("two")
	break
default:
	log("def")
}
const { x: renamed, y = 5, ...rest } = { x: 1, z: 9 }
const [p, , q = 4] = [7]
log(renamed + y + rest.z + p + q)
class Counter {
	constructor(start) { this.n = start }
	inc() { this.n++; return this.n }
	get value() { return this.n }
}
const c = new Counter(10)
c.inc()
log(c.value)
function* gen() { yield 1; yield 2 }
for (const g of gen()) log(g)
const dbl = v => v * 2
const blk = v => { const t = v + 1; return t * 2 }
log(dbl(3) + blk(3))
try {
	throw new Error("boom")
} catch (e) {
	log(e.message)
} finally {
	log("fin")
}
const fn = function named(n) { return n <= 1 ? 1 : n * named(n - 1) }
log(fn(4))
out.join("|")
`

func run(t *testing.T, src string) sobek.Value {
	t.Helper()
	vm := sobek.New()
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(vm.Set(HookName, func(sobek.FunctionCall) sobek.Value { return sobek.Undefined() }))
	v, err := vm.RunScript("test.js", src)
	must(err)
	return v
}

func TestSemanticsPreserved(t *testing.T) {
	res, err := File("test.js", gnarly, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := run(t, gnarly).String()
	got := run(t, res.Source).String()
	if got != want {
		t.Errorf("instrumented output differs:\n want %q\n got  %q", want, got)
	}
}

func TestLineNumbersPreserved(t *testing.T) {
	res, err := File("test.js", gnarly, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(res.Source, "\n") != strings.Count(gnarly, "\n") {
		t.Errorf("line count changed: %d -> %d", strings.Count(gnarly, "\n"), strings.Count(res.Source, "\n"))
	}
}

func TestDirectivePrologueStaysFirst(t *testing.T) {
	res, err := File("test.js", gnarly, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Source, `"use strict";`) {
		t.Errorf("directive no longer first: %q", res.Source[:40])
	}
}

func TestScopesContainExpectedNames(t *testing.T) {
	src := `function f(p1, p2) {
	let local = 1;
	__stop__();
	return local + p1 + p2;
}
var g = 10;
f(1, 2);
`
	res, err := File("test.js", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	// Find the scope used inside f: it must contain params, the local and the global.
	found := false
	for _, names := range res.Scopes {
		set := map[string]bool{}
		for _, n := range names {
			set[n] = true
		}
		if set["p1"] && set["p2"] && set["local"] && set["g"] && set["f"] {
			found = true
		}
	}
	if !found {
		t.Errorf("no scope with expected names; scopes: %v", res.Scopes)
	}
	for l := 2; l <= 4; l++ {
		if !res.Lines[l] {
			t.Errorf("line %d not instrumented; lines: %v", l, res.Lines)
		}
	}
}

// Whitespace (including newlines) and comments may separate a braceless body
// from its trailing semicolon; the closing brace must still absorb the ';' or
// the following `else` is a syntax error.
func TestCloseOffNewlineAndComments(t *testing.T) {
	src := `var r = "";
function b() { r += "b" }
function c() { r += "c" }
var a = true;
if (a) b()
; else c();
a = false;
if (a) b() /* x */ ; else c();
if (a) b() // note
; else c();
r;
`
	res, err := File("test.js", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := run(t, src).String()
	got := run(t, res.Source).String()
	if got != want {
		t.Errorf("instrumented output differs:\n want %q\n got  %q\nsource:\n%s", want, got, res.Source)
	}
}

// The directive-prologue carve-out applies only where prologues can occur
// (program and function bodies). A leading string literal in a plain block or
// switch case is an ordinary statement and must be hooked.
func TestNonDirectiveStringsGetHooks(t *testing.T) {
	src := `function f(x) {
	"use strict";
	switch (x) {
	case 1:
		"marker";
		x = x + 10;
	}
	{
		"in block";
		x = x + 1;
	}
	return x;
}
f(1);
`
	res, err := File("test.js", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Lines[2] {
		t.Errorf(`function-body "use strict" was hooked`)
	}
	for _, l := range []int{5, 9} { // "marker" and "in block"
		if !res.Lines[l] {
			t.Errorf("string statement on line %d not hooked; lines: %v", l, res.Lines)
		}
	}
	if want, got := run(t, src).String(), run(t, res.Source).String(); got != want {
		t.Errorf("instrumented output differs: want %q got %q", want, got)
	}
}

// OrigCol must map instrumented-source columns back to the on-disk source:
// columns after injected text shift back by its length, columns inside
// injected text clamp to the statement the injection fronts.
func TestOrigCol(t *testing.T) {
	src := "var a = 1; var b = 2;\nif (a) b = 3;\n"
	res, err := File("test.js", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(res.Source, "\n")
	col := func(line int, sub string) int {
		i := strings.Index(lines[line-1], sub)
		if i < 0 {
			t.Fatalf("%q not on line %d of %q", sub, line, lines[line-1])
		}
		return i + 1
	}
	for _, tc := range []struct {
		line     int
		sub      string
		wantCol  int
		wantWhat string
	}{
		{1, "var a", 1, "first statement"},
		{1, "var b", 12, "second statement, shifted by one hook"},
		{1, HookName, 1, "inside injected text clamps to the statement"},
		{2, "b = 3", 8, "brace-wrapped body: shifted by '{' + hook"},
	} {
		if got := res.OrigCol(tc.line, col(tc.line, tc.sub)); got != tc.wantCol {
			t.Errorf("%s: OrigCol(%d, %d) = %d, want %d", tc.wantWhat, tc.line, col(tc.line, tc.sub), got, tc.wantCol)
		}
	}
	// A line with no insertions maps to itself.
	if got := res.OrigCol(99, 7); got != 7 {
		t.Errorf("untouched line: OrigCol = %d, want 7", got)
	}
}

func TestDebuggerStatementForced(t *testing.T) {
	res, err := File("test.js", "var x = 1;\ndebugger;\nx = 2;\n", 7)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Source, ",1);") {
		t.Errorf("debugger statement did not produce forced hook: %q", res.Source)
	}
	// The scriptID must be baked into every hook as its first argument.
	if !strings.Contains(res.Source, HookName+"(7,") {
		t.Errorf("scriptID missing from hooks: %q", res.Source)
	}
}
