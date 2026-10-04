// Package herdrtest is a fake herdr server on a temp Unix socket. It replays
// the responses captured in testdata/herdr/socket/*.ndjson and records the
// requests it receives.
package herdrtest

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// Request is one request the server received.
type Request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Server is a running fake.
type Server struct {
	Path string

	mu       sync.Mutex
	replies  map[string]json.RawMessage // key: method + canonical params
	requests []Request
	handlers map[string]Handler
	ln       net.Listener
}

// Handler answers one method in place of the fixtures: a result object, or
// a non-empty error code.
type Handler func(params json.RawMessage) (result any, code string)

// Handle sends every request for method to fn instead of the fixtures. Use
// it where one request must get different answers over time, such as a
// pane.read before and after a prompt. fn runs without the server's lock, so
// it may call Requests.
func (s *Server) Handle(method string, fn Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handlers == nil {
		s.handlers = map[string]Handler{}
	}
	s.handlers[method] = fn
}

// FixtureDir returns testdata/herdr/socket, located from this source file.
func FixtureDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "herdr", "socket")
}

func key(method string, params json.RawMessage) string {
	var v any
	if len(params) > 0 && json.Unmarshal(params, &v) == nil {
		b, _ := json.Marshal(v) // maps marshal with sorted keys
		return method + " " + string(b)
	}
	return method + " {}"
}

// New starts a server that replays the named fixtures (file names without the
// directory, for example "session-snapshot.ndjson"). With no names it loads
// every fixture; when two fixtures share a method and params, the later name
// wins, so pass names explicitly to pick one. Requests with no fixture get a
// "no_fixture" error response. The server stops when the test ends.
func New(t testing.TB, names ...string) *Server {
	t.Helper()
	dir := FixtureDir()
	if len(names) == 0 {
		m, err := filepath.Glob(filepath.Join(dir, "*.ndjson"))
		if err != nil || len(m) == 0 {
			t.Fatalf("herdrtest: no fixtures in %s", dir)
		}
		for _, f := range m {
			names = append(names, filepath.Base(f))
		}
	}
	s := &Server{replies: map[string]json.RawMessage{}}
	for _, n := range names {
		f, err := os.Open(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("herdrtest: %v", err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		var lines [][]byte
		for sc.Scan() {
			lines = append(lines, append([]byte(nil), sc.Bytes()...))
		}
		f.Close()
		if len(lines) != 2 {
			t.Fatalf("herdrtest: %s: want 2 lines (request, response), got %d", n, len(lines))
		}
		var req Request
		if err := json.Unmarshal(lines[0], &req); err != nil {
			t.Fatalf("herdrtest: %s: %v", n, err)
		}
		s.replies[key(req.Method, req.Params)] = lines[1]
	}

	dirTmp, err := os.MkdirTemp("", "herdrtest")
	if err != nil {
		t.Fatal(err)
	}
	s.Path = filepath.Join(dirTmp, "herdr.sock")
	s.ln, err = net.Listen("unix", s.Path)
	if err != nil {
		t.Fatal(err)
	}
	go s.serve()
	t.Cleanup(func() {
		s.ln.Close()
		os.RemoveAll(dirTmp)
	})
	return s
}

// Requests returns a copy of the requests received so far.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// RequestsFor returns the requests received so far for method, in order.
func (s *Server) RequestsFor(method string) []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Request
	for _, r := range s.requests {
		if r.Method == method {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	// Like the real herdr, answer one request and close the connection.
	sc := bufio.NewScanner(c)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	if !sc.Scan() {
		return
	}
	var req Request
	if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
		c.Write([]byte(`{"id":"","error":{"code":"invalid_request","message":"bad json"}}` + "\n"))
		return
	}
	s.mu.Lock()
	s.requests = append(s.requests, req)
	fn := s.handlers[req.Method]
	reply, ok := s.replies[key(req.Method, req.Params)]
	s.mu.Unlock()
	var out []byte
	switch {
	case fn != nil:
		res, code := fn(req.Params)
		if code != "" {
			out, _ = json.Marshal(map[string]any{"id": req.ID, "error": map[string]string{"code": code, "message": code}})
		} else {
			out, _ = json.Marshal(map[string]any{"id": req.ID, "result": res})
		}
	case !ok:
		out, _ = json.Marshal(map[string]any{"id": req.ID, "error": map[string]string{
			"code": "no_fixture", "message": "no fixture for " + key(req.Method, req.Params)}})
	default:
		// Keep the captured response, swap in the caller's request ID.
		var m map[string]json.RawMessage
		if json.Unmarshal(reply, &m) != nil {
			return
		}
		id, _ := json.Marshal(req.ID)
		m["id"] = id
		out, _ = json.Marshal(m)
	}
	c.Write(append(out, '\n'))
}
