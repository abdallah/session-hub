package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestReplayEachOp(t *testing.T) {
	tests := []struct {
		name       string
		item       Item
		wantMethod string
		wantPath   string
		wantBody   string
	}{
		{"upsert", Item{Op: OpUpsert, Body: []byte(`{"id":"s1","agent":"claude","source":"hooks"}`)},
			"POST", "/v1/sessions", `{"id":"s1","agent":"claude","source":"hooks"}`},
		{"herdr_sessions", Item{Op: OpHerdrSessions, Body: []byte(`{"herdr_session":"default","sessions":[]}`)},
			"PUT", "/v1/machines/self/herdr-sessions", `{"herdr_session":"default","sessions":[]}`},
		{"event", Item{Op: OpEvent, SessionID: "s1", Body: []byte(`{"kind":"prompt","source":"hooks","ts":"2026-09-30T10:00:00Z"}`)},
			"POST", "/v1/sessions/s1/events", `{"kind":"prompt","source":"hooks","ts":"2026-09-30T10:00:00Z"}`},
		{"report", Item{Op: OpReport, SessionID: "s1", Body: []byte(`{"done":["a"],"in_flight":[],"waiting_on":[]}`)},
			"POST", "/v1/sessions/s1/report", `{"done":["a"],"in_flight":[],"waiting_on":[]}`},
		{"title", Item{Op: OpTitle, SessionID: "s1", Body: []byte(`{"title":"T"}`)},
			"POST", "/v1/sessions/s1/title", `{"title":"T"}`},
		{"digest", Item{Op: OpDigest, SessionID: "sess-1", Body: []byte(`{"as_of":"2026-09-30T10:00:00Z","tokens":{"input":1,"output":2,"cache_read":3,"cache_write":4}}`)},
			"PUT", "/v1/sessions/sess-1/digest", `{"as_of":"2026-09-30T10:00:00Z","tokens":{"input":1,"output":2,"cache_read":3,"cache_write":4},"bad_lines":0}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath, gotBody, gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				gotMethod, gotPath, gotBody, gotAuth = r.Method, r.URL.Path, string(b), r.Header.Get("Authorization")
				w.Write([]byte(`{}`))
			}))
			defer srv.Close()
			c, _ := New(Config{ServerURL: srv.URL, Token: "tok"})
			if err := Replay(context.Background(), c, tc.item); err != nil {
				t.Fatal(err)
			}
			if gotMethod != tc.wantMethod || gotPath != tc.wantPath || gotBody != tc.wantBody || gotAuth != "Bearer tok" {
				t.Errorf("got %s %s %s auth=%q", gotMethod, gotPath, gotBody, gotAuth)
			}
		})
	}
}

func TestReplayUnsendableItems(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer srv.Close()
	c, _ := New(Config{ServerURL: srv.URL})
	for name, it := range map[string]Item{
		"unknown op":    {Op: "nope", SessionID: "s1"},
		"bad body":      {Op: OpReport, SessionID: "s1", Body: []byte(`{"done":`)},
		"digest no id":  {Op: OpDigest, Body: []byte(`{"as_of":"2026-09-30T10:00:00Z"}`)},
		"missing id":    {Op: OpTitle, Body: []byte(`{"title":"T"}`)},
		"empty body":    {Op: OpUpsert},
		"wrong shape":   {Op: OpReport, SessionID: "s1", Body: []byte(`{"done":"x"}`)},
		"event no body": {Op: OpEvent, SessionID: "s1"},
	} {
		err := Replay(context.Background(), c, it)
		if err == nil || IsRetryable(err) {
			t.Errorf("%s: err = %v, retryable = %v; want a non-retryable error", name, err, IsRetryable(err))
		}
	}
	if calls != 0 {
		t.Errorf("server got %d requests for unsendable items", calls)
	}
}

func TestErrorClassification(t *testing.T) {
	status := func(n int) error { return &StatusError{Status: n, Message: "x"} }
	tests := []struct {
		name      string
		err       error
		retryable bool
		notFound  bool
	}{
		{"nil", nil, false, false},
		{"network", errors.New("dial tcp: connection refused"), true, false},
		{"deadline", context.DeadlineExceeded, true, false},
		{"wrapped deadline", fmt.Errorf("post: %w", context.DeadlineExceeded), true, false},
		{"400", status(400), false, false},
		{"401", status(401), false, false},
		{"403", status(403), false, false},
		{"404", status(404), false, true},
		{"wrapped 404", fmt.Errorf("x: %w", status(404)), false, true},
		{"408", status(408), true, false},
		{"409", status(409), false, false},
		{"429", status(429), true, false},
		{"500", status(500), true, false},
		{"503", status(503), true, false},
	}
	for _, tc := range tests {
		if got := IsRetryable(tc.err); got != tc.retryable {
			t.Errorf("%s: IsRetryable = %v, want %v", tc.name, got, tc.retryable)
		}
		if got := IsNotFound(tc.err); got != tc.notFound {
			t.Errorf("%s: IsNotFound = %v, want %v", tc.name, got, tc.notFound)
		}
	}
}

func TestReplayClassifiesRealResponses(t *testing.T) {
	codes := map[int]bool{400: false, 404: false, 429: true, 500: true, 502: true}
	for code, want := range codes {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
		}))
		c, _ := New(Config{ServerURL: srv.URL})
		err := Replay(context.Background(), c, Item{Op: OpTitle, SessionID: "s", Body: []byte(`{"title":"T"}`)})
		if IsRetryable(err) != want {
			t.Errorf("%d: retryable = %v, want %v (%v)", code, IsRetryable(err), want, err)
		}
		srv.Close()
	}
	// Server gone: network error is retryable.
	srv := httptest.NewServer(http.NotFoundHandler())
	c, _ := New(Config{ServerURL: srv.URL})
	srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Replay(ctx, c, Item{Op: OpTitle, SessionID: "s", Body: []byte(`{"title":"T"}`)}); !IsRetryable(err) {
		t.Errorf("refused connection: err = %v, want retryable", err)
	}
}
