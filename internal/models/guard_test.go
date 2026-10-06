package models

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pyck-ai/jev-cli/internal/openrouter"
)

// guardEnv is a Guard over the testdata catalog with a temp cache dir, a
// settable clock and captured warnings.
type guardEnv struct {
	g    *Guard
	get  *fakeGetter
	warn *bytes.Buffer
	dir  string
	now  time.Time
	mu   sync.Mutex
}

func newGuardEnv(t *testing.T) *guardEnv {
	t.Helper()
	e := &guardEnv{get: &fakeGetter{}, warn: &bytes.Buffer{}, dir: t.TempDir(), now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	e.g = NewGuard(e.get, e.opts()).WithWarnWriter(e.warn)
	return e
}

func (e *guardEnv) opts() Options {
	return Options{CacheDir: e.dir, Now: func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }}
}

func (e *guardEnv) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

func choiceQ(n int) openrouter.Question {
	c := map[string]string{}
	for i := 0; i < n; i++ {
		c[string(rune('a'+i%26))+strings.Repeat("x", i/26)] = "d"
	}
	return openrouter.Question{Type: "choice", Instructions: "i", Criteria: c}
}

func req(model string, state any, qs map[string]openrouter.Question) *openrouter.Request {
	return &openrouter.Request{Model: model, Questions: qs, State: state}
}

func (e *guardEnv) reject(r *openrouter.Request, msg string) {
	body := []byte(`{"error":{"code":400,"message":"` + msg + `"}}`)
	e.g.AfterAsk(context.Background(), r, 400, body, nil, &openrouter.APIError{StatusCode: 400, Code: 400, Message: msg}, time.Millisecond)
}

func TestGuard_ContextOverflowAborts(t *testing.T) {
	e := newGuardEnv(t)
	big := strings.Repeat("x", 65536*4+100) // liquid/d1: 65536 tokens
	r := req("liquid/d1", big, map[string]openrouter.Question{"q": {Type: "noul", Instructions: "i", Criteria: map[string]string{"a": "b"}}})
	err := e.g.BeforeAsk(context.Background(), r)
	var pe *PreflightError
	if !errors.As(err, &pe) || pe.Kind != PreflightContext {
		t.Fatalf("err = %v, want context PreflightError", err)
	}
	if pe.ContextLength != 65536 || pe.EstimatedTokens <= 65536 {
		t.Errorf("pe = %+v", pe)
	}
	for _, want := range []string{"liquid/d1", "65536", "jev models"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err.Error(), want)
		}
	}
	// Small state is fine; unknown context length (respan) never trips.
	r.State = "short"
	if err := e.g.BeforeAsk(context.Background(), r); err != nil {
		t.Errorf("small request: %v", err)
	}
	if err := e.g.BeforeAsk(context.Background(), req("respan/span-01", big, r.Questions)); err != nil {
		t.Errorf("unknown context length must not abort: %v", err)
	}
	// Alias resolves to its target's context length (32000 tokens).
	if err := e.g.BeforeAsk(context.Background(), req("~typesafe/jev-latest", strings.Repeat("x", 32000*4+10), r.Questions)); err == nil {
		t.Error("alias should be checked against target context")
	}
}

func TestEstimateTokens(t *testing.T) {
	r := req("m", "abcdefgh", map[string]openrouter.Question{"q": {Type: "noul", Instructions: "12345678", Criteria: []string{}}})
	// state "abcdefgh" = 10 bytes JSON, 8 instructions, criteria "[]" = 2 -> 20/4
	if got := EstimateTokens(r); got != 5 {
		t.Errorf("estimate = %d, want 5", got)
	}
}

func TestGuard_UnknownModelWarnsOnceAndAllows(t *testing.T) {
	e := newGuardEnv(t)
	r := req("nosuch/model", "s", map[string]openrouter.Question{"q": {Type: "noul"}})
	for i := 0; i < 3; i++ {
		if err := e.g.BeforeAsk(context.Background(), r); err != nil {
			t.Fatalf("unknown model must be allowed: %v", err)
		}
	}
	out := e.warn.String()
	if strings.Count(out, "is not in OpenRouter's decision-model catalog") != 1 {
		t.Errorf("warning count wrong: %q", out)
	}
	if !strings.Contains(out, `jev: model "nosuch/model" is not in OpenRouter's decision-model catalog (cached `) || !strings.Contains(out, "sending anyway") {
		t.Errorf("warning = %q", out)
	}
	if e.get.calls != 1 {
		t.Errorf("catalog loaded %d times, want 1 (lazy, once)", e.get.calls)
	}
}

func TestGuard_CatalogFailureIsPermissive(t *testing.T) {
	e := newGuardEnv(t)
	e.get.err = errors.New("offline")
	r := req("liquid/d1", strings.Repeat("x", 1_000_000), map[string]openrouter.Question{"q": {Type: "noul"}})
	for i := 0; i < 3; i++ {
		if err := e.g.BeforeAsk(context.Background(), r); err != nil {
			t.Fatalf("permissive guard errored: %v", err)
		}
	}
	e.reject(r, "boom") // must not panic or learn
	if n := strings.Count(e.warn.String(), "catalog unavailable"); n != 1 {
		t.Errorf("warnings = %d (%q), want 1", n, e.warn.String())
	}
	if _, err := os.Stat(e.dir + "/limits.json"); err == nil {
		t.Error("limits learned without a catalog")
	}
}

func TestGuard_LearnedLimitBlocksEquivalentOnly(t *testing.T) {
	e := newGuardEnv(t)
	ctx := context.Background()
	msg := `Tev1 needs between 2 and 20 options per question; question "c" has 30`
	bad := req("liquid/d1", "some state", map[string]openrouter.Question{"c": choiceQ(30)})
	if err := e.g.BeforeAsk(ctx, bad); err != nil {
		t.Fatal(err)
	}
	e.reject(bad, msg)

	// Equivalent request (different content, same shape): blocked, quoting the provider.
	same := req("liquid/d1", "other text!", map[string]openrouter.Question{"zz": choiceQ(30)})
	err := e.g.BeforeAsk(ctx, same)
	var pe *PreflightError
	if !errors.As(err, &pe) || pe.Kind != PreflightLearned || pe.Learned == nil {
		t.Fatalf("err = %v, want learned PreflightError", err)
	}
	for _, want := range []string{"liquid/d1 rejected an equivalent request at", "Tev1 needs between 2 and 20 options", "learned limit; expires", "jev models --refresh clears it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err.Error(), want)
		}
	}

	// A different signature goes through.
	cases := map[string]*openrouter.Request{
		"fewer options":   req("liquid/d1", "some state", map[string]openrouter.Question{"c": choiceQ(5)}),
		"other type":      req("liquid/d1", "some state", map[string]openrouter.Question{"c": {Type: "noul", Instructions: "i", Criteria: map[string]string{"a": "b"}}}),
		"much smaller":    req("liquid/d1", "s", map[string]openrouter.Question{"c": choiceQ(30)}),
		"much larger":     req("liquid/d1", strings.Repeat("x", 5000), map[string]openrouter.Question{"c": choiceQ(30)}),
		"object state":    req("liquid/d1", map[string]any{"input": "x"}, map[string]openrouter.Question{"c": choiceQ(30)}),
		"two questions":   req("liquid/d1", "some state", map[string]openrouter.Question{"c": choiceQ(30), "d": choiceQ(30)}),
		"different model": req("typesafe/jev-1.13", "some state", map[string]openrouter.Question{"c": choiceQ(30)}),
	}
	for name, r := range cases {
		if err := e.g.BeforeAsk(ctx, r); err != nil {
			t.Errorf("%s: unexpectedly blocked: %v", name, err)
		}
	}
}

func TestGuard_AliasAndCanonicalShareLimit(t *testing.T) {
	e := newGuardEnv(t)
	ctx := context.Background()
	qs := map[string]openrouter.Question{"s": {Type: "noul", Instructions: "i", Criteria: "plain"}}
	e.reject(req("~typesafe/jev-latest", "x", qs), "plain strings")
	if err := e.g.BeforeAsk(ctx, req("typesafe/jev-1.13", "y", qs)); err == nil {
		t.Error("limit learned via alias should apply to the resolved model")
	}
}

func TestGuard_DoesNotExistIsNotLearned(t *testing.T) {
	e := newGuardEnv(t)
	ctx := context.Background()
	r := req("nosuch/model", "s", map[string]openrouter.Question{"q": {Type: "noul"}})
	_ = e.g.BeforeAsk(ctx, r) // loads the catalog
	e.reject(r, "Model nosuch/model does not exist")
	if err := e.g.BeforeAsk(ctx, r); err != nil {
		t.Errorf("not-in-catalog 400 was learned: %v", err)
	}
	if _, err := os.Stat(e.dir + "/limits.json"); err == nil {
		t.Error("limits.json written for an unknown model")
	}
	// Non-400 statuses are never learned either.
	known := req("liquid/d1", "s", map[string]openrouter.Question{"q": {Type: "noul"}})
	e.g.AfterAsk(ctx, known, 500, []byte("x"), nil, errors.New("x"), 0)
	e.g.AfterAsk(ctx, known, 200, []byte("{}"), &openrouter.Response{}, nil, 0)
	if err := e.g.BeforeAsk(ctx, known); err != nil {
		t.Errorf("non-400 was learned: %v", err)
	}
}

func TestGuard_RealMessagesLearned(t *testing.T) {
	e := newGuardEnv(t)
	ctx := context.Background()
	for i, msg := range []string{
		`Respan only accepts noul questions whose instructions and criteria are plain strings (question "s")`,
		`Respan state must be a string or an object with only input (a message array) and output (a message), where each message has a string content and a role of system, user, assistant or tool, and the output role is assistant`,
	} {
		r := req("respan/span-01", "x", map[string]openrouter.Question{"s": choiceQ(2 + i)})
		e.reject(r, msg)
		err := e.g.BeforeAsk(ctx, r)
		if err == nil || !strings.Contains(err.Error(), msg) {
			t.Errorf("msg %d: err = %v", i, err)
		}
	}
}

func TestGuard_Expiry(t *testing.T) {
	e := newGuardEnv(t)
	ctx := context.Background()
	r := req("liquid/d1", "s", map[string]openrouter.Question{"c": choiceQ(30)})
	e.reject(r, "too many")
	e.advance(DefaultTTL - time.Minute)
	if err := e.g.BeforeAsk(ctx, r); err == nil {
		t.Fatal("limit should still apply just before expiry")
	}
	e.advance(2 * time.Minute)
	if err := e.g.BeforeAsk(ctx, r); err != nil {
		t.Errorf("limit should have expired: %v", err)
	}
}

func TestGuard_PersistenceRoundTripAndRefreshClears(t *testing.T) {
	e := newGuardEnv(t)
	ctx := context.Background()
	r := req("liquid/d1", "s", map[string]openrouter.Question{"c": choiceQ(30)})
	e.reject(r, "too many")

	// A fresh Guard (new process) over the same dir sees it.
	g2 := NewGuard(&fakeGetter{}, e.opts()).WithWarnWriter(&bytes.Buffer{})
	var pe *PreflightError
	if err := g2.BeforeAsk(ctx, r); !errors.As(err, &pe) || pe.Learned.Message != "too many" || pe.Learned.Model != "liquid/d1" {
		t.Fatalf("persisted limit not applied: %v", err)
	}
	data, err := os.ReadFile(e.dir + "/limits.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"some`) || !strings.Contains(string(data), "signature") {
		t.Errorf("limits.json = %s", data)
	}

	// Re-learning the same shape replaces rather than duplicates.
	e.reject(r, "too many again")
	if n := len(readLimits(e.dir+"/limits.json", e.now, DefaultTTL)); n != 1 {
		t.Errorf("limits = %d, want 1", n)
	}

	if err := ClearLimits(e.opts()); err != nil {
		t.Fatal(err)
	}
	if err := ClearLimits(e.opts()); err != nil {
		t.Errorf("clearing twice: %v", err)
	}
	g3 := NewGuard(&fakeGetter{}, e.opts()).WithWarnWriter(&bytes.Buffer{})
	if err := g3.BeforeAsk(ctx, r); err != nil {
		t.Errorf("cleared limit still applies: %v", err)
	}
}

func TestGuard_CorruptLimitsFileIgnoredAndConcurrent(t *testing.T) {
	e := newGuardEnv(t)
	ctx := context.Background()
	if err := os.WriteFile(e.dir+"/limits.json", []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := req("liquid/d1", "s", map[string]openrouter.Question{"c": choiceQ(30)})
	if err := e.g.BeforeAsk(ctx, r); err != nil {
		t.Fatalf("corrupt file must not block: %v", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rr := req("liquid/d1", strings.Repeat("x", 1<<(i%10)), map[string]openrouter.Question{"c": choiceQ(2 + i%5)})
			_ = e.g.BeforeAsk(ctx, rr)
			e.reject(rr, "m")
		}(i)
	}
	wg.Wait()
	e.reject(r, "m")
	if err := e.g.BeforeAsk(ctx, r); err == nil {
		t.Error("expected learned limit after concurrent writes")
	}
}

func TestSignature_ShapeOnly(t *testing.T) {
	a := Signature(req("m", map[string]any{"b": 1, "a": 2}, map[string]openrouter.Question{"x": choiceQ(3), "y": {Type: "noul"}}))
	b := Signature(req("m", map[string]any{"a": 9, "b": 8}, map[string]openrouter.Question{"p": {Type: "noul"}, "q": choiceQ(3)}))
	if a != b {
		t.Errorf("same shape, different signature:\n%s\n%s", a, b)
	}
	for _, want := range []string{"types=choice,noul", "state=object{a,b}", "maxopts=3", "questions=2"} {
		if !strings.Contains(a, want) {
			t.Errorf("signature %q lacks %q", a, want)
		}
	}
	if Signature(req("m", []any{"x"}, nil)) == Signature(req("m", "x", nil)) {
		t.Error("array and string state must differ")
	}
}
