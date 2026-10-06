package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pyck-ai/jev-cli/internal/openrouter"
)

// PreflightError is returned (from openrouter.Client.Ask) when the Guard
// refuses to send a request. Nothing was sent. Tools wrap Ask errors with
// %w and print err.Error(), so the message reaches the user unchanged;
// callers that want to tell a refusal from a provider error can use
// errors.As.
type PreflightError struct {
	Model   string // the model as requested
	Kind    string // PreflightContext or PreflightLearned
	Message string
	// Context-overflow details (Kind == PreflightContext).
	EstimatedTokens int
	ContextLength   int
	// The matching limit (Kind == PreflightLearned).
	Learned *LearnedLimit
}

// PreflightError kinds.
const (
	PreflightContext = "context"
	PreflightLearned = "learned"
)

func (e *PreflightError) Error() string { return e.Message }

// Guard is an openrouter.Hook that refuses requests that are known to
// fail, using only facts learned from the API:
//
//   - the model's context_length from the catalog (Load): a request whose
//     estimated size exceeds it is refused;
//   - limits learned from the provider's own HTTP 400 answers, persisted
//     in limits.json (see LearnedLimit) for the catalog TTL.
//
// Everything else is allowed: a catalog failure makes the Guard
// permissive (one warning), and an unknown model is sent anyway (one
// warning per slug), because the provider's 400 is the authority. Safe
// for concurrent use.
type Guard struct {
	getter Getter
	opts   Options
	warn   io.Writer

	loadOnce sync.Once
	catalog  *Catalog // nil: unavailable (permissive)

	mu     sync.Mutex // guards warned and limits.json access
	warned map[string]bool
}

// NewGuard builds a Guard. g is normally the *openrouter.Client; opts is
// the same Options `jev models` uses (cache dir, TTL, clock).
func NewGuard(g Getter, opts Options) *Guard {
	return &Guard{getter: g, opts: opts, warn: os.Stderr, warned: map[string]bool{}}
}

// WithWarnWriter redirects the Guard's warnings (default os.Stderr).
func (g *Guard) WithWarnWriter(w io.Writer) *Guard {
	g.warn = w
	return g
}

var _ openrouter.Hook = (*Guard)(nil)

// warnOnce prints msg once per key.
func (g *Guard) warnOnce(key, msg string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.warned[key] {
		return
	}
	g.warned[key] = true
	fmt.Fprintln(g.warn, msg)
}

func (g *Guard) load(ctx context.Context) *Catalog {
	g.loadOnce.Do(func() {
		res, err := Load(ctx, g.getter, g.opts)
		if err != nil {
			g.warnOnce("catalog", fmt.Sprintf("jev: warning: model catalog unavailable (%v); sending without pre-send checks", err))
			return
		}
		if res.Warning != nil {
			g.warnOnce("catalog-warning", fmt.Sprintf("jev: warning: %v", res.Warning))
		}
		g.catalog = res.Catalog
	})
	return g.catalog
}

// BeforeAsk implements openrouter.Hook.
func (g *Guard) BeforeAsk(ctx context.Context, req *openrouter.Request) error {
	cat := g.load(ctx)
	if cat == nil {
		return nil
	}
	m, ok := cat.Resolve(req.Model)
	if !ok {
		age := g.opts.now().Sub(cat.FetchedAt).Round(time.Minute)
		g.warnOnce("unknown:"+req.Model, fmt.Sprintf(
			"jev: model %q is not in OpenRouter's decision-model catalog (cached %s ago); sending anyway", req.Model, age))
		return nil
	}

	if m.ContextLength > 0 {
		if est := EstimateTokens(req); est > m.ContextLength {
			return &PreflightError{
				Model: req.Model, Kind: PreflightContext, EstimatedTokens: est, ContextLength: m.ContextLength,
				Message: fmt.Sprintf("%s: request is about %d input tokens, over the model's context length of %d; shorten the input or pick a larger-context model (see `jev models`, then --model)",
					req.Model, est, m.ContextLength),
			}
		}
	}

	sig := Signature(req)
	g.mu.Lock()
	limits := g.readLimitsLocked()
	g.mu.Unlock()
	for _, l := range limits {
		if l.Model == m.ID && l.Signature == sig {
			l := l
			return &PreflightError{
				Model: req.Model, Kind: PreflightLearned, Learned: &l,
				Message: fmt.Sprintf("%s rejected an equivalent request at %s: \"%s\" (learned limit; expires %s; jev models --refresh clears it)",
					m.ID, l.LearnedAt.Local().Format(time.RFC3339), l.Message, l.Expires(g.opts.ttl()).Local().Format(time.RFC3339)),
			}
		}
	}
	return nil
}

// AfterAsk implements openrouter.Hook: it learns from an HTTP 400 for a
// model that is in the catalog. A 400 for a model the catalog does not
// know ("Model X does not exist") is never learned.
func (g *Guard) AfterAsk(ctx context.Context, req *openrouter.Request, status int, body []byte, _ *openrouter.Response, err error, _ time.Duration) {
	if status != 400 {
		return
	}
	cat := g.load(ctx)
	if cat == nil {
		return
	}
	m, ok := cat.Resolve(req.Model)
	if !ok {
		return
	}
	msg := strings.TrimSpace(string(body))
	var apiErr *openrouter.APIError
	if errors.As(err, &apiErr) && apiErr.Message != "" {
		msg = apiErr.Message
	}
	entry := LearnedLimit{Model: m.ID, Signature: Signature(req), Message: msg, LearnedAt: g.opts.now().UTC()}

	path, perr := g.opts.LimitsPath()
	if perr != nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	limits := readLimits(path, g.opts.now(), g.opts.ttl())
	kept := limits[:0]
	for _, l := range limits {
		if !(l.Model == entry.Model && l.Signature == entry.Signature) {
			kept = append(kept, l)
		}
	}
	_ = writeLimits(path, append(kept, entry)) // never fatal
}

func (g *Guard) readLimitsLocked() []LearnedLimit {
	path, err := g.opts.LimitsPath()
	if err != nil {
		return nil
	}
	return readLimits(path, g.opts.now(), g.opts.ttl())
}

// EstimateTokens is a rough input-token estimate: the JSON size of the
// state plus the size of every question's instructions and criteria,
// divided by 4 (rounded up). It is deliberately crude; it only has to be
// good enough to catch requests that are several times too large.
func EstimateTokens(req *openrouter.Request) int {
	n := 0
	if b, err := json.Marshal(req.State); err == nil {
		n += len(b)
	}
	for _, q := range req.Questions {
		n += len(q.Instructions)
		if b, err := json.Marshal(q.Criteria); err == nil {
			n += len(b)
		}
	}
	return (n + 3) / 4
}

// Signature describes the SHAPE of a request, never its content, so a
// learned 400 can be matched against later, equivalent requests without
// storing (or leaking) anything the user sent. It is:
//
//   - the sorted set of question types;
//   - the state kind: string, array, or object with its sorted top-level keys;
//   - the maximum number of options among choice questions (exact);
//   - a log2 bucket of the state's JSON byte size;
//   - the question count.
//
// The size bucket matters: some providers answer an oversized input with
// an opaque 400 (jaredpalmer/kev-4b returns code 20015 "The parameter is
// invalid" for big input). Without the bucket, learning that 400 would
// also block every small request to the same model for 24h. With it, a
// request is only blocked when it has the same shape AND is within a
// factor of two in size of the one that failed (same power-of-two bucket).
func Signature(req *openrouter.Request) string {
	typeSet := map[string]bool{}
	maxOpts := 0
	for _, q := range req.Questions {
		typeSet[q.Type] = true
		if q.Type == "choice" {
			if n := criteriaCount(q.Criteria); n > maxOpts {
				maxOpts = n
			}
		}
	}
	types := make([]string, 0, len(typeSet))
	for t := range typeSet {
		types = append(types, t)
	}
	sort.Strings(types)

	size := 0
	kind := "null"
	if b, err := json.Marshal(req.State); err == nil {
		size = len(b)
		kind = stateKind(b)
	}
	return fmt.Sprintf("types=%s;state=%s;maxopts=%d;size=2^%d;questions=%d",
		strings.Join(types, ","), kind, maxOpts, bits.Len(uint(size)), len(req.Questions))
}

func criteriaCount(c any) int {
	b, err := json.Marshal(c)
	if err != nil {
		return 0
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(b, &m) == nil {
		return len(m)
	}
	var a []json.RawMessage
	if json.Unmarshal(b, &a) == nil {
		return len(a)
	}
	return 0
}

func stateKind(b []byte) string {
	var v any
	if json.Unmarshal(b, &v) != nil {
		return "unknown"
	}
	switch t := v.(type) {
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return "object{" + strings.Join(keys, ",") + "}"
	case nil:
		return "null"
	default:
		return "other"
	}
}
