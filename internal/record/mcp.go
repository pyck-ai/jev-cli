package record

import (
	"context"
	"encoding/json"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCPMiddleware returns receiving middleware that records the connecting
// client (on "initialize") and one tool_call per "tools/call", and puts the
// call's id into ctx so the systemone hook can correlate. On a nil Recorder
// it returns a pass-through.
func (r *Recorder) MCPMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		if r == nil {
			return next
		}
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			// initialize itself bypasses receiving middleware, so the client
			// record is written on the first request that finds the session
			// initialized.
			r.noteClient(req)
			if method == "tools/call" {
				return r.recordToolCall(ctx, method, req, next)
			}
			return next(ctx, method, req)
		}
	}
}

// noteClient writes the client record once, as soon as the session's
// InitializeParams are known.
func (r *Recorder) noteClient(req mcp.Request) {
	ss, ok := req.GetSession().(*mcp.ServerSession)
	if !ok || ss == nil {
		return
	}
	ip := ss.InitializeParams()
	if ip == nil || ip.ClientInfo == nil {
		return
	}
	r.clientOnce.Do(func() {
		r.Write(Record{Kind: KindClient, Client: &ClientInfo{Name: ip.ClientInfo.Name, Version: ip.ClientInfo.Version}})
	})
}

func (r *Recorder) recordToolCall(ctx context.Context, method string, req mcp.Request, next mcp.MethodHandler) (mcp.Result, error) {
	id := NewCallID()
	rec := Record{Kind: KindToolCall, CallID: id, Transport: "mcp"}
	if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && p != nil {
		rec.Tool = p.Name
		if len(p.Arguments) > 0 {
			rec.Arguments = append(json.RawMessage(nil), p.Arguments...)
		}
	}
	if ss, ok := req.GetSession().(*mcp.ServerSession); ok && ss != nil {
		if ip := ss.InitializeParams(); ip != nil && ip.ClientInfo != nil {
			rec.Client = &ClientInfo{Name: ip.ClientInfo.Name, Version: ip.ClientInfo.Version}
		}
	}

	start := time.Now()
	res, err := next(WithCallID(ctx, id), method, req)
	rec.LatencyMS = ms(time.Since(start))

	isErr := err != nil
	if err != nil {
		rec.Error = err.Error()
	}
	if cr, ok := res.(*mcp.CallToolResult); ok && cr != nil {
		tr := &ToolResult{Structured: cr.StructuredContent}
		for _, c := range cr.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				tr.Text = append(tr.Text, tc.Text)
			}
		}
		rec.Result = tr
		if cr.IsError {
			isErr = true
			if rec.Error == "" && len(tr.Text) > 0 {
				rec.Error = tr.Text[0]
			}
		}
	}
	rec.IsError = &isErr
	r.Write(rec)
	return res, err
}
