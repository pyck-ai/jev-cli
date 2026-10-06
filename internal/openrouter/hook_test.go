package openrouter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type recHook struct {
	mu     sync.Mutex
	name   string
	log    *[]string
	abort  error
	status int
	body   string
	resp   *Response
	err    error
	after  int
	model  string
}

func (h *recHook) BeforeAsk(_ context.Context, req *Request) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.log = append(*h.log, "before:"+h.name)
	h.model = req.Model
	return h.abort
}

func (h *recHook) AfterAsk(_ context.Context, _ *Request, status int, body []byte, resp *Response, err error, latency time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	*h.log = append(*h.log, "after:"+h.name)
	h.after++
	h.status, h.body, h.resp, h.err = status, string(body), resp, err
}

func hookClient(srv *httptest.Server) *Client {
	return NewClientWithEndpoint("k", srv.URL, RetryPolicy{MaxAttempts: 1})
}

func TestHook_AbortSendsNothing(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	var log []string
	want := errors.New("nope")
	first := &recHook{name: "a", log: &log, abort: want}
	second := &recHook{name: "b", log: &log}
	c := hookClient(srv)
	c.AddHook(first)
	c.AddHook(second)
	_, err := c.Ask(context.Background(), "m/x", map[string]Question{"q": {Type: "noul"}}, "s", 0)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if hits != 0 {
		t.Errorf("server hit %d times, want 0", hits)
	}
	if len(log) != 1 || log[0] != "before:a" {
		t.Errorf("log = %v, want only before:a", log)
	}
	if first.after != 0 || second.after != 0 {
		t.Error("AfterAsk must not run for an aborted call")
	}
}

func TestHook_AfterAsk400AndOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"code":400,"message":"bad shape"}}`))
	}))
	defer srv.Close()
	var log []string
	a := &recHook{name: "a", log: &log}
	b := &recHook{name: "b", log: &log}
	c := hookClient(srv)
	c.AddHook(a)
	c.AddHook(b)
	_, err := c.Ask(context.Background(), "m/x", map[string]Question{"q": {Type: "noul"}}, "s", 0)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v", err)
	}
	if a.status != 400 || a.resp != nil || a.body == "" || !errors.As(a.err, &apiErr) || apiErr.Message != "bad shape" {
		t.Errorf("AfterAsk got status=%d resp=%v body=%q err=%v", a.status, a.resp, a.body, a.err)
	}
	want := []string{"before:a", "before:b", "after:a", "after:b"}
	if len(log) != 4 {
		t.Fatalf("log = %v", log)
	}
	for i := range want {
		if log[i] != want[i] {
			t.Fatalf("log = %v, want %v", log, want)
		}
	}
}

func TestHook_AfterAsk200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"m/x","answers":{"q":{"type":"noul","noul":1}}}`))
	}))
	defer srv.Close()
	var log []string
	h := &recHook{name: "a", log: &log}
	c := hookClient(srv)
	c.AddHook(h)
	out, err := c.Ask(context.Background(), "m/x", map[string]Question{"q": {Type: "noul"}}, "s", 0)
	if err != nil || out == nil {
		t.Fatalf("Ask: %v", err)
	}
	if h.status != 200 || h.resp != out || h.err != nil || h.body == "" || h.model != "m/x" {
		t.Errorf("AfterAsk got status=%d resp=%v err=%v body=%q", h.status, h.resp, h.err, h.body)
	}
}

func TestHook_TransportErrorStatusZero(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	var log []string
	h := &recHook{name: "a", log: &log}
	c := hookClient(srv)
	c.AddHook(h)
	_, err := c.Ask(context.Background(), "m/x", map[string]Question{"q": {Type: "noul"}}, "s", 0)
	if err == nil || h.status != 0 || h.err == nil || h.after != 1 {
		t.Errorf("err=%v status=%d hookErr=%v after=%d", err, h.status, h.err, h.after)
	}
}
