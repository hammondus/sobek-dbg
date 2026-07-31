// Debug Adapter Protocol server. Hand-rolled rather than a library dependency:
// the wire format is Content-Length-framed JSON (identical to LSP) and this
// debugger needs ~15 of the protocol's requests, so the whole thing fits in
// one auditable file. Spec: https://microsoft.github.io/debug-adapter-protocol/
package dbg

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

// renderTemplate turns an evaluation result into a display string plus a
// typeof, separated by \x01. Errors come back as \x00ERR:<name> so the
// variables view can distinguish "not accessible here" (TDZ, wrong frame,
// over-collected pattern name) from a real value.
//
// The newline before the closing paren is load-bearing: without it an
// expression ending in a line comment (`total // check`) would swallow the
// `);` and turn valid input into a SyntaxError.
const renderTemplate = `(function () {
	try {
		var __v = (%s
);
		var __t = typeof __v;
		var __s;
		if (__t === "function") { __s = String(__v).split("{")[0].trim() || "function"; }
		else if (__t === "string") { __s = JSON.stringify(__v); }
		else if (__v !== null && __t === "object") {
			try { __s = JSON.stringify(__v); if (__s === undefined) __s = String(__v); }
			catch (__e) { __s = String(__v); }
		}
		else { __s = String(__v); }
		if (__s.length > 500) __s = __s.slice(0, 500) + "…";
		return __t + "\u0001" + __s;
	} catch (__e) { return "\u0000ERR:" + __e.name; }
})()`

type Server struct {
	D *Debugger

	mu   sync.Mutex // guards conn + seq
	conn net.Conn
	seq  int
}

// ListenAndServe accepts DAP clients one at a time, forever. Each VS Code
// debug session is one connection; when it disconnects the script keeps
// running and a new session may attach.
func (s *Server) ListenAndServe(addr string) error {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.Serve(l)
}

func (s *Server) Serve(l net.Listener) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return err
		}
		// One client at a time, newest wins: a fresh attach kicks any existing
		// session — which may be a half-dead socket the OS hasn't timed out —
		// rather than queueing unanswered behind it in the listen backlog.
		// Closing the old conn unblocks its handler's read; that handler's
		// teardown sees it is no longer current and leaves the new session
		// alone. (A reply the old handler is writing at this instant would go
		// to the new conn; its unknown request_seq makes the client drop it.)
		s.mu.Lock()
		if s.conn != nil {
			s.conn.Close()
		}
		s.conn = conn
		s.seq = 0
		s.mu.Unlock()
		go s.handle(conn)
	}
}

type request struct {
	Seq       int             `json:"seq"`
	Type      string          `json:"type"`
	Command   string          `json:"command"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	defer func() {
		// Tear down only if this is still the active session — a newer
		// connection may have kicked this one and taken over the debugger.
		s.mu.Lock()
		current := s.conn == conn
		if current {
			s.conn = nil
		}
		s.mu.Unlock()
		if current {
			s.D.detach() // drop sink, cancel pending step/pause, release the VM
		}
	}()

	s.D.setEventSink(func(event string, body map[string]any) {
		s.send(map[string]any{"type": "event", "event": event, "body": body})
	})

	r := bufio.NewReader(conn)
	for {
		req, err := readMessage(r)
		if err != nil {
			// Client vanished mid-session: teardown lets the script run free.
			return
		}
		if disconnect := s.dispatch(req); disconnect {
			return
		}
	}
}

func readMessage(r *bufio.Reader) (*request, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length:"); ok {
			if _, err := fmt.Sscanf(strings.TrimSpace(v), "%d", &length); err != nil {
				return nil, err
			}
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("missing Content-Length")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var req request
	if err := json.Unmarshal(buf, &req); err != nil {
		return nil, err
	}
	return &req, nil
}

func (s *Server) send(msg map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return
	}
	s.seq++
	msg["seq"] = s.seq
	b, err := json.Marshal(msg)
	if err != nil {
		return
	}
	fmt.Fprintf(s.conn, "Content-Length: %d\r\n\r\n%s", len(b), b)
}

func (s *Server) reply(req *request, body any) {
	s.send(map[string]any{
		"type": "response", "request_seq": req.Seq, "command": req.Command,
		"success": true, "body": body,
	})
}

func (s *Server) fail(req *request, msg string) {
	s.send(map[string]any{
		"type": "response", "request_seq": req.Seq, "command": req.Command,
		"success": false, "message": msg,
	})
}

func (s *Server) event(name string, body map[string]any) {
	s.send(map[string]any{"type": "event", "event": name, "body": body})
}

func (s *Server) dispatch(req *request) (disconnect bool) {
	switch req.Command {
	case "initialize":
		s.reply(req, map[string]any{
			"supportsConfigurationDoneRequest": true,
			"supportsConditionalBreakpoints":   true,
			"supportsEvaluateForHovers":        true,
			"supportsSetVariable":              true,
			"supportsTerminateRequest":         false,
		})
		s.event("initialized", map[string]any{})

	case "attach", "launch":
		var args struct {
			StopOnEntry bool `json:"stopOnEntry"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		s.D.mu.Lock()
		s.D.stopOnEntry = args.StopOnEntry
		s.D.mu.Unlock()
		s.reply(req, map[string]any{})

	case "setBreakpoints":
		var args struct {
			Source struct {
				Path string `json:"path"`
			} `json:"source"`
			Breakpoints []struct {
				Line      int    `json:"line"`
				Condition string `json:"condition"`
			} `json:"breakpoints"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			s.fail(req, err.Error())
			return
		}
		lines := make([]int, len(args.Breakpoints))
		conds := make([]string, len(args.Breakpoints))
		for i, b := range args.Breakpoints {
			lines[i], conds[i] = b.Line, b.Condition
		}
		verified := s.D.setBreakpoints(args.Source.Path, lines, conds)
		bps := make([]map[string]any, len(lines))
		for i := range lines {
			bps[i] = map[string]any{"verified": verified[i], "line": lines[i]}
		}
		s.reply(req, map[string]any{"breakpoints": bps})

	case "setExceptionBreakpoints":
		s.reply(req, map[string]any{})

	case "configurationDone":
		s.D.markConfigured()
		s.reply(req, map[string]any{})

	case "threads":
		s.reply(req, map[string]any{"threads": []map[string]any{{"id": 1, "name": "main"}}})

	case "stackTrace":
		frames, _, paused := s.D.snapshot()
		if !paused {
			s.fail(req, "not paused")
			return
		}
		out := make([]map[string]any, 0, len(frames))
		for i, f := range frames {
			// Frame positions are computed against the instrumented source;
			// lines match the file on disk but columns are shifted by the
			// injected hook text. Map them back before the editor places the
			// execution caret.
			pos := f.Position()
			out = append(out, map[string]any{
				"id":   i,
				"name": f.FuncName(),
				"source": map[string]any{
					"name": filepath.Base(f.SrcName()),
					"path": f.SrcName(),
				},
				"line":   pos.Line,
				"column": s.D.origColumn(f.SrcName(), pos.Line, pos.Column),
			})
		}
		s.reply(req, map[string]any{"stackFrames": out, "totalFrames": len(out)})

	case "scopes":
		var args struct {
			FrameID int `json:"frameId"`
		}
		_ = json.Unmarshal(req.Arguments, &args)
		scopes := []map[string]any{}
		if args.FrameID == 0 {
			// Variables can only be resolved in the innermost frame: that is
			// where the paused eval thunk lives. Outer frames still show
			// their source position.
			scopes = append(scopes, map[string]any{
				"name": "Locals", "variablesReference": 1, "expensive": false,
			})
		}
		s.reply(req, map[string]any{"scopes": scopes})

	case "variables":
		_, scope, paused := s.D.snapshot()
		if !paused {
			s.fail(req, "not paused")
			return
		}
		vars := []map[string]any{}
		for _, name := range scope {
			typ, val, ok := s.render(name)
			if !ok {
				continue // not accessible in this frame (TDZ, other scope)
			}
			vars = append(vars, map[string]any{
				"name": name, "value": val, "type": typ, "variablesReference": 0,
			})
		}
		s.reply(req, map[string]any{"variables": vars})

	case "setVariable":
		var args struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			s.fail(req, err.Error())
			return
		}
		// Name should come from our own variables list, but the client sends
		// it back verbatim — verify it really is a plain identifier before
		// splicing it into an assignment. The newline before the inner close
		// paren keeps a value ending in a line comment from swallowing it.
		if !isIdentifier(args.Name) {
			s.fail(req, "not a variable")
			return
		}
		typ, val, ok := s.render(fmt.Sprintf("%s = (%s\n)", args.Name, args.Value))
		if !ok {
			s.fail(req, val)
			return
		}
		s.reply(req, map[string]any{"value": val, "type": typ, "variablesReference": 0})

	case "evaluate":
		var args struct {
			Expression string `json:"expression"`
		}
		if err := json.Unmarshal(req.Arguments, &args); err != nil {
			s.fail(req, err.Error())
			return
		}
		typ, val, ok := s.render(args.Expression)
		if !ok {
			s.fail(req, val) // val carries the error name
			return
		}
		s.reply(req, map[string]any{"result": val, "type": typ, "variablesReference": 0})

	case "continue":
		s.reply(req, map[string]any{"allThreadsContinued": true})
		s.D.resume(modeRun)

	case "next":
		s.reply(req, map[string]any{})
		s.D.resume(modeStepOver)

	case "stepIn":
		s.reply(req, map[string]any{})
		s.D.resume(modeStepIn)

	case "stepOut":
		s.reply(req, map[string]any{})
		s.D.resume(modeStepOut)

	case "pause":
		s.D.requestPause()
		s.reply(req, map[string]any{})

	case "disconnect":
		s.reply(req, map[string]any{})
		return true // handle()'s teardown resumes the script

	default:
		s.fail(req, "unsupported request: "+req.Command)
	}
	return false
}

// isIdentifier reports whether s looks like a plain JS identifier — good
// enough to reject anything that could smuggle extra syntax into the
// setVariable assignment splice (reserved words simply fail their eval).
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		if r == '_' || r == '$' || unicode.IsLetter(r) || (i > 0 && unicode.IsDigit(r)) {
			continue
		}
		return false
	}
	return true
}

// render evaluates an expression in the paused frame and formats it for
// display. ok=false means the expression failed; val then holds the error.
func (s *Server) render(expr string) (typ, val string, ok bool) {
	v, err := s.D.evalInFrame(fmt.Sprintf(renderTemplate, expr))
	if err != nil {
		return "", err.Error(), false
	}
	out := v.String()
	if rest, isErr := strings.CutPrefix(out, "\x00ERR:"); isErr {
		return "", rest, false
	}
	if t, rest, found := strings.Cut(out, "\x01"); found {
		return t, rest, true
	}
	return "", out, true
}
