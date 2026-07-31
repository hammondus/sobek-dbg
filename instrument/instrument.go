// Package instrument rewrites JavaScript source so that a host-installed
// hook function is called before every statement. The rewrite is purely
// textual (offset-based splicing driven by sobek's own parser), inserts no
// newlines, and therefore preserves all line numbers — stack traces and
// breakpoints refer to the original source.
//
// Injected before each statement:
//
//	;__dbg(<scriptID>,<line>,<scopeID>,__x=>eval(__x));
//
// The arrow thunk gives the debugger direct eval in the paused scope: it can
// read and write locals and evaluate arbitrary expressions. The scriptID is
// the caller-chosen identity of this file (a debugger managing several
// scripts uses it to find the right breakpoint map and scope table without
// capturing a stack on the fast path). The scopeID keys into Result.Scopes,
// the statically-computed list of variable names visible at that point (used
// to populate the Variables pane; values are fetched through the thunk, one
// guarded eval per name).
package instrument

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/grafana/sobek/ast"
	"github.com/grafana/sobek/file"
	"github.com/grafana/sobek/parser"
)

// HookName is the global the instrumented code calls. The host must define it
// before running the program; script code must not.
const HookName = "__dbg"

type Result struct {
	Source string       // instrumented source, same line count as input
	Lines  map[int]bool // lines containing at least one hook (valid breakpoint targets)
	Scopes [][]string   // scopeID -> names visible there, innermost first
	shifts map[int][]colShift
}

// colShift records one insertion on a line: the 1-based original column it was
// spliced at and how many bytes it added. Kept in splice order so OrigCol can
// map columns in the instrumented text back to the on-disk source.
type colShift struct{ col, n int }

// OrigCol maps a column reported against the instrumented source back to the
// original source. Columns inside injected text (e.g. a frame pointing at the
// hook call itself) map to the column the injection was spliced at — the start
// of the real statement. Lines are unchanged by instrumentation, so no line
// mapping is needed.
func (r *Result) OrigCol(line, col int) int {
	shift := 0
	for _, s := range r.shifts[line] {
		start := s.col + shift
		if col < start {
			break
		}
		if col < start+s.n {
			return s.col
		}
		shift += s.n
	}
	return col - shift
}

// File instruments src. filename is used for parse errors and must match the
// name the host later compiles the instrumented source under, so that stack
// frames map back to the file the editor has open. scriptID is baked into
// every emitted hook call as its first argument.
func File(filename, src string, scriptID int) (*Result, error) {
	fs := &file.FileSet{}
	prog, err := parser.ParseFile(fs, filename, src, 0)
	if err != nil {
		return nil, err
	}

	w := &walker{src: src, fs: fs}
	// Region 0 is the whole program: top-level declarations land here.
	w.regions = append(w.regions, &region{start: 0, end: len(src)})
	w.visit(prog)

	for _, d := range w.decls {
		w.assign(d)
	}

	res := &Result{Lines: map[int]bool{}, shifts: map[int][]colShift{}}
	scopeIDs := map[string]int{}
	var sb strings.Builder
	sort.SliceStable(w.ins, func(i, j int) bool {
		if w.ins[i].off != w.ins[j].off {
			return w.ins[i].off < w.ins[j].off
		}
		return w.ins[i].pri < w.ins[j].pri
	})
	last := 0
	for _, in := range w.ins {
		sb.WriteString(src[last:in.off])
		last = in.off
		before := sb.Len()
		switch in.kind {
		case insOpen:
			sb.WriteByte('{')
		case insClose:
			sb.WriteByte('}')
		case insHook:
			names := w.scopeAt(in.off)
			key := strings.Join(names, ",")
			id, ok := scopeIDs[key]
			if !ok {
				id = len(res.Scopes)
				scopeIDs[key] = id
				res.Scopes = append(res.Scopes, names)
			}
			forced := ""
			if in.forced {
				forced = ",1"
			}
			fmt.Fprintf(&sb, ";%s(%d,%d,%d,__x=>eval(__x)%s);", HookName, scriptID, in.line, id, forced)
			res.Lines[in.line] = true
		}
		pos := fs.Position(file.Idx(in.off + 1))
		res.shifts[pos.Line] = append(res.shifts[pos.Line], colShift{col: pos.Column, n: sb.Len() - before})
	}
	sb.WriteString(src[last:])
	res.Source = sb.String()
	return res, nil
}

// region is a variable-visibility range: the whole program, or a function
// including its parameter list (so parameter bindings fall inside by offset).
type region struct {
	start, end int
	names      []string
	seen       map[string]bool
}

func (r *region) add(name string) {
	if r.seen == nil {
		r.seen = map[string]bool{}
	}
	if !r.seen[name] {
		r.seen[name] = true
		r.names = append(r.names, name)
	}
}

type decl struct {
	name    string
	off     int
	exclude *region // a function's own name is declared in the *enclosing* scope
}

type insKind int

const (
	insOpen insKind = iota // '{' when brace-wrapping a single-statement body
	insHook
	insClose // '}'
)

type insertion struct {
	off, pri int
	kind     insKind
	line     int
	forced   bool // debugger; statement: stop regardless of breakpoints
}

type walker struct {
	src     string
	fs      *file.FileSet
	regions []*region
	decls   []decl
	ins     []insertion
}

// file.Idx is 1-based; byte offsets are 0-based.
func off(i file.Idx) int { return int(i) - 1 }

func (w *walker) line(i file.Idx) int { return w.fs.Position(i).Line }

func (w *walker) assign(d decl) {
	var best *region
	for _, r := range w.regions {
		if r == d.exclude || d.off < r.start || d.off >= r.end {
			continue
		}
		if best == nil || r.end-r.start < best.end-best.start {
			best = r
		}
	}
	if best != nil {
		best.add(d.name)
	}
}

// scopeAt returns names visible at a hook, innermost region first.
func (w *walker) scopeAt(o int) []string {
	var rs []*region
	for _, r := range w.regions {
		if o >= r.start && o < r.end {
			rs = append(rs, r)
		}
	}
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].end-rs[i].start < rs[j].end-rs[j].start })
	var names []string
	seen := map[string]bool{}
	for _, r := range rs {
		for _, n := range r.names {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	return names
}

func (w *walker) hook(s ast.Statement) {
	switch s.(type) {
	case *ast.EmptyStatement, *ast.FunctionDeclaration:
		// Empty: nothing to observe. Function declarations are hoisted; their
		// list position executes nothing, so a hook there is just step noise.
		return
	}
	if s.Idx0() == 0 {
		return // parser left no position (seen on `else if` nodes)
	}
	_, forced := s.(*ast.DebuggerStatement)
	o := off(s.Idx0())
	w.ins = append(w.ins, insertion{off: o, pri: 1, kind: insHook, line: w.line(s.Idx0()), forced: forced})
}

// closeOff extends a wrap's closing-brace offset past a trailing semicolon,
// which syntactically belongs to the wrapped statement but is not covered by
// its Idx1. Leaving it outside would turn `if (c) x(); else` into
// `if (c) {x()}; else` — a syntax error. Whitespace (including newlines) and
// comments may sit between the statement and its semicolon; they are skipped,
// never consumed — insertions still add no newlines, so line numbers hold.
func (w *walker) closeOff(o int) int {
	i := o
	for i < len(w.src) {
		switch {
		case w.src[i] == ' ' || w.src[i] == '\t' || w.src[i] == '\r' || w.src[i] == '\n':
			i++
		case strings.HasPrefix(w.src[i:], "//"):
			for i < len(w.src) && w.src[i] != '\n' {
				i++
			}
		case strings.HasPrefix(w.src[i:], "/*"):
			end := strings.Index(w.src[i+2:], "*/")
			if end < 0 {
				return o // unterminated comment: parser would have rejected it
			}
			i += 2 + end + 2
		case w.src[i] == ';':
			return i + 1
		default:
			return o
		}
	}
	return o
}

// stmtList hooks each statement in a list. prologue is true only where a
// directive prologue is grammatically possible (program and function bodies):
// there, leading string-literal statements ("use strict") are left unhooked so
// they stay first. In plain blocks and switch cases a leading string is an
// ordinary statement and gets its hook like any other.
func (w *walker) stmtList(list []ast.Statement, prologue bool) {
	for _, s := range list {
		if prologue {
			if es, ok := s.(*ast.ExpressionStatement); ok {
				if _, isStr := es.Expression.(*ast.StringLiteral); isStr {
					continue // don't hook, nothing to recurse into
				}
			}
			prologue = false
		}
		w.hook(s)
		w.visit(s)
	}
}

// body handles the single-statement (braceless) body of if/else/loops/with:
// it brace-wraps the statement so a hook can be injected in front of it
// without changing which statement the construct governs.
func (w *walker) body(s ast.Statement) {
	if s == nil {
		return
	}
	switch s.(type) {
	case *ast.BlockStatement:
		w.visit(s)
		return
	case *ast.IfStatement:
		// `else if` — treat as a continuation of the same statement, like
		// debuggers conventionally do. (sobek also leaves Idx0 unset here, so
		// there is no reliable position to hook.)
		w.visit(s)
		return
	case *ast.LabelledStatement:
		// `while (c) l: continue l` — wrapping is fine, but a hook between
		// label and statement is not; hook goes before the label.
		w.ins = append(w.ins,
			insertion{off: off(s.Idx0()), pri: 0, kind: insOpen},
			insertion{off: off(s.Idx0()), pri: 1, kind: insHook, line: w.line(s.Idx0())},
			insertion{off: w.closeOff(off(s.Idx1())), pri: 3, kind: insClose})
		w.visit(s)
		return
	}
	if s.Idx0() == 0 {
		w.visit(s)
		return
	}
	w.ins = append(w.ins, insertion{off: off(s.Idx0()), pri: 0, kind: insOpen})
	w.hook(s)
	w.ins = append(w.ins, insertion{off: w.closeOff(off(s.Idx1())), pri: 3, kind: insClose})
	w.visit(s)
}

// function records a visibility region spanning the whole function literal —
// parameter list included, so parameter bindings assign to it by offset.
func (w *walker) function(start, end file.Idx) *region {
	r := &region{start: off(start), end: off(end)}
	w.regions = append(w.regions, r)
	return r
}

func (w *walker) visit(n ast.Node) {
	if n == nil {
		return
	}
	switch t := n.(type) {
	case *ast.Program:
		w.stmtList(t.Body, true)
	case *ast.BlockStatement:
		w.stmtList(t.List, false)
	case *ast.SwitchStatement:
		w.reflectWalk(reflect.ValueOf(t.Discriminant))
		for _, c := range t.Body {
			w.reflectWalk(reflect.ValueOf(c.Test))
			w.stmtList(c.Consequent, false)
		}
	case *ast.IfStatement:
		w.reflectWalk(reflect.ValueOf(t.Test))
		w.body(t.Consequent)
		w.body(t.Alternate)
	case *ast.ForStatement:
		w.reflectWalk(reflect.ValueOf(t.Initializer))
		w.reflectWalk(reflect.ValueOf(t.Test))
		w.reflectWalk(reflect.ValueOf(t.Update))
		w.body(t.Body)
	case *ast.ForInStatement:
		w.reflectWalk(reflect.ValueOf(t.Into))
		w.reflectWalk(reflect.ValueOf(t.Source))
		w.body(t.Body)
	case *ast.ForOfStatement:
		w.reflectWalk(reflect.ValueOf(t.Into))
		w.reflectWalk(reflect.ValueOf(t.Source))
		w.body(t.Body)
	case *ast.WhileStatement:
		w.reflectWalk(reflect.ValueOf(t.Test))
		w.body(t.Body)
	case *ast.DoWhileStatement:
		w.body(t.Body)
		w.reflectWalk(reflect.ValueOf(t.Test))
	case *ast.WithStatement:
		w.reflectWalk(reflect.ValueOf(t.Object))
		w.body(t.Body)
	case *ast.FunctionLiteral:
		r := w.function(t.Idx0(), t.Idx1())
		if t.Name != nil {
			r.add(t.Name.Name.String()) // named function expression: visible inside itself
		}
		w.reflectWalk(reflect.ValueOf(t.ParameterList))
		w.stmtList(t.Body.List, true) // function body: directive prologue possible
	case *ast.ArrowFunctionLiteral:
		w.function(t.Idx0(), t.Idx1())
		w.reflectWalk(reflect.ValueOf(t.ParameterList))
		if bs, ok := t.Body.(*ast.BlockStatement); ok {
			w.stmtList(bs.List, true) // function body: directive prologue possible
		} else {
			// Concise body (x => expr): no statement to hook — stepping does
			// not stop inside; nested functions in the expression still get
			// instrumented.
			w.reflectWalk(reflect.ValueOf(t.Body))
		}
	case *ast.FunctionDeclaration:
		// The function's name binds in the enclosing scope, not its own.
		r := w.function(t.Function.Idx0(), t.Function.Idx1())
		if t.Function.Name != nil {
			w.decls = append(w.decls, decl{name: t.Function.Name.Name.String(), off: off(t.Idx0()), exclude: r})
		}
		w.reflectWalk(reflect.ValueOf(t.Function.ParameterList))
		w.stmtList(t.Function.Body.List, true) // function body: directive prologue possible
	case *ast.ClassDeclaration:
		if t.Class.Name != nil {
			w.decls = append(w.decls, decl{name: t.Class.Name.Name.String(), off: off(t.Idx0())})
		}
		w.reflectWalk(reflect.ValueOf(t.Class))
	case *ast.Binding:
		w.patternNames(t.Target)
		w.reflectWalk(reflect.ValueOf(t.Initializer))
	case *ast.ForDeclaration: // for (let x of ...)
		w.patternNames(t.Target)
	case *ast.CatchStatement:
		if t.Parameter != nil {
			w.patternNames(t.Parameter)
		}
		w.visit(t.Body)
	case *ast.DebuggerStatement, *ast.EmptyStatement, *ast.BranchStatement:
		// leaves
	default:
		// Walk the node's fields — not the node itself, which would loop
		// straight back into visit.
		rv := reflect.ValueOf(n)
		if rv.Kind() == reflect.Pointer && !rv.IsNil() {
			w.reflectWalk(rv.Elem())
		}
	}
}

// patternNames records the identifiers a binding target declares, handling
// destructuring patterns. Offsets place each name in its enclosing region.
func (w *walker) patternNames(n ast.Node) {
	switch t := n.(type) {
	case *ast.Identifier:
		w.decls = append(w.decls, decl{name: t.Name.String(), off: off(t.Idx)})
	case *ast.ObjectPattern:
		for _, p := range t.Properties {
			switch pt := p.(type) {
			case *ast.PropertyShort:
				w.decls = append(w.decls, decl{name: pt.Name.Name.String(), off: off(pt.Name.Idx)})
			case *ast.PropertyKeyed:
				w.patternNames(pt.Value) // key is not a binding
			case *ast.SpreadElement:
				w.patternNames(pt.Expression)
			}
		}
		if t.Rest != nil {
			w.patternNames(t.Rest)
		}
	case *ast.ArrayPattern:
		for _, e := range t.Elements {
			if e != nil {
				w.patternNames(e)
			}
		}
		if t.Rest != nil {
			w.patternNames(t.Rest)
		}
	case *ast.Binding: // pattern element with default value
		w.patternNames(t.Target)
		w.reflectWalk(reflect.ValueOf(t.Initializer))
	}
}

var nodeType = reflect.TypeFor[ast.Node]()

// reflectWalk descends into arbitrary AST structure, dispatching every
// ast.Node it finds back through visit. Using reflection means new or
// forgotten node kinds still get traversed (worst case they are stepped
// through rather than specially handled). Runs once per script in debug
// mode, so speed is irrelevant.
func (w *walker) reflectWalk(v reflect.Value) {
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return
		}
		if v.Type().Implements(nodeType) {
			w.visit(v.Interface().(ast.Node))
			return
		}
		w.reflectWalk(v.Elem())
	case reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			w.reflectWalk(v.Index(i))
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			// DeclarationList fields alias Binding nodes that also appear in
			// statement position; walking them twice would double-instrument
			// initializers.
			if t.Field(i).Name == "DeclarationList" || t.Field(i).Name == "File" {
				continue
			}
			w.reflectWalk(v.Field(i))
		}
	}
}
