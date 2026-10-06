package record

import (
	"context"
	"encoding/json"
	"time"

	"github.com/pyck-ai/jev-cli/internal/openrouter"
)

// hook is the openrouter.Hook that writes systemone records. It wraps an
// inner hook (the preflight guard) so a BeforeAsk veto is recorded too.
type hook struct {
	r     *Recorder
	inner openrouter.Hook
}

// WrapHook returns an openrouter.Hook that delegates to inner (which may be
// nil) and records every Ask: those that reached the network (in AfterAsk)
// and those inner's BeforeAsk aborted (as preflight_error). Recording never
// changes inner's verdict. On a nil Recorder it returns inner unchanged.
func (r *Recorder) WrapHook(inner openrouter.Hook) openrouter.Hook {
	if r == nil {
		return inner
	}
	return &hook{r: r, inner: inner}
}

func (h *hook) BeforeAsk(ctx context.Context, req *openrouter.Request) error {
	if h.inner == nil {
		return nil
	}
	start := time.Now()
	err := h.inner.BeforeAsk(ctx, req)
	if err != nil {
		rec := h.base(ctx, req)
		rec.PreflightError = err.Error()
		rec.Error = err.Error()
		rec.LatencyMS = ms(time.Since(start))
		h.r.Write(rec)
	}
	return err
}

func (h *hook) AfterAsk(ctx context.Context, req *openrouter.Request, status int, body []byte, resp *openrouter.Response, err error, latency time.Duration) {
	rec := h.base(ctx, req)
	rec.HTTPStatus = &status
	if len(body) > 0 {
		if json.Valid(body) {
			rec.Response = json.RawMessage(append([]byte(nil), body...))
		} else {
			rec.Response, _ = json.Marshal(string(body))
		}
	}
	if resp != nil {
		rec.ResponseModel = resp.Model
		rec.Provider = resp.Provider
		if resp.Usage != nil {
			rec.Usage = resp.Usage
		}
	}
	if err != nil {
		rec.Error = err.Error()
	}
	rec.LatencyMS = ms(latency)
	h.r.Write(rec)
	if h.inner != nil {
		h.inner.AfterAsk(ctx, req, status, body, resp, err, latency)
	}
}

func (h *hook) base(ctx context.Context, req *openrouter.Request) Record {
	rec := Record{Kind: KindSystemOne, CallID: CallID(ctx), Model: req.Model}
	if b, err := json.Marshal(req); err == nil {
		rec.Request = b
	}
	return rec
}
