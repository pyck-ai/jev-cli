package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Endpoint resolves where and how a model is called.
type Endpoint struct {
	BaseURL string
	APIKey  string
	// Direct sends every model to BaseURL+"/chat/completions" (OpenRouter).
	Direct bool
}

// URL applies the per-model route rule. On the LiteLLM proxy, Anthropic models
// (ids starting "anthropic/" or "~anthropic/") use /v1, everything else goes
// through the proxy's OpenRouter passthrough.
func (e Endpoint) URL(model string) string {
	base := strings.TrimRight(e.BaseURL, "/")
	if e.Direct {
		return base + "/chat/completions"
	}
	if strings.HasPrefix(model, "anthropic/") || strings.HasPrefix(model, "~anthropic/") {
		return base + "/v1/chat/completions"
	}
	return base + "/openrouter/api/v1/chat/completions"
}

// Observation is what one request produced.
type Observation struct {
	Tool             string // bare tool name, or "none"
	RawTool          string // name exactly as the model returned it
	Args             map[string]any
	ArgsRaw          string
	FinishReason     string
	Cost             float64
	PromptTokens     int
	CompletionTokens int
	Text             string // assistant text content, if any
}

type chatResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   any `json:"content"`
			ToolCalls []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments any    `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int      `json:"prompt_tokens"`
		CompletionTokens int      `json:"completion_tokens"`
		Cost             *float64 `json:"cost"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// parseResponse extracts the first tool call of the first choice. A response
// with no tool call is not an error: Tool is "none". An API-level error body
// is returned as an error.
func parseResponse(body []byte, prefix string) (Observation, error) {
	var r chatResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return Observation{}, fmt.Errorf("decode response: %w", err)
	}
	if r.Error != nil && len(r.Choices) == 0 {
		return Observation{}, fmt.Errorf("api error: %s", r.Error.Message)
	}
	if len(r.Choices) == 0 {
		return Observation{}, fmt.Errorf("response has no choices")
	}
	obs := Observation{
		Tool:             "none",
		PromptTokens:     r.Usage.PromptTokens,
		CompletionTokens: r.Usage.CompletionTokens,
	}
	if r.Usage.Cost != nil {
		obs.Cost = *r.Usage.Cost
	}
	ch := r.Choices[0]
	obs.FinishReason = ch.FinishReason
	if s, ok := ch.Message.Content.(string); ok {
		obs.Text = s
	}
	if len(ch.Message.ToolCalls) == 0 {
		return obs, nil
	}
	fn := ch.Message.ToolCalls[0].Function
	obs.RawTool = fn.Name
	obs.Tool = bareName(fn.Name, prefix)
	switch a := fn.Arguments.(type) {
	case string:
		obs.ArgsRaw = a
		if strings.TrimSpace(a) != "" {
			// Malformed JSON leaves Args nil; schema validation reports it.
			_ = json.Unmarshal([]byte(a), &obs.Args)
		}
	case map[string]any:
		obs.Args = a
		b, _ := json.Marshal(a)
		obs.ArgsRaw = string(b)
	}
	return obs, nil
}

// Caller sends requests to the endpoint.
type Caller struct {
	HTTP      *http.Client
	Endpoint  Endpoint
	Prefix    string
	MaxTokens int
	Retries   int

	mu     sync.Mutex
	noTemp map[string]bool // models that rejected temperature
}

type chatTool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

func (c *Caller) buildBody(model, system, prompt string, fns []Function, withTemp bool) ([]byte, error) {
	tools := make([]chatTool, len(fns))
	for i, f := range fns {
		tools[i] = chatTool{Type: "function", Function: f}
	}
	body := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": prompt},
		},
		"tools":       tools,
		"tool_choice": "auto",
		"max_tokens":  c.MaxTokens,
	}
	if withTemp {
		body["temperature"] = 0
	}
	return json.Marshal(body)
}

func (c *Caller) skipTemp(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.noTemp[model]
}

func (c *Caller) markNoTemp(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.noTemp == nil {
		c.noTemp = map[string]bool{}
	}
	c.noTemp[model] = true
}

// Call runs one trial request, retrying 429/5xx up to c.Retries times and
// dropping temperature once if the model rejects it.
func (c *Caller) Call(ctx context.Context, model, system, prompt string, fns []Function) (Observation, error) {
	withTemp := !c.skipTemp(model)
	var lastErr error
	for attempt := 0; attempt <= c.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Observation{}, ctx.Err()
			case <-time.After(time.Duration(attempt) * 2 * time.Second):
			}
		}
		payload, err := c.buildBody(model, system, prompt, fns, withTemp)
		if err != nil {
			return Observation{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint.URL(model), bytes.NewReader(payload))
		if err != nil {
			return Observation{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.Endpoint.APIKey)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, rerr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(body), 200))
			continue
		case resp.StatusCode == http.StatusBadRequest && withTemp && strings.Contains(strings.ToLower(string(body)), "temperature"):
			c.markNoTemp(model)
			withTemp = false
			attempt-- // not a real retry
			continue
		case resp.StatusCode >= 400:
			return Observation{}, fmt.Errorf("http %d: %s", resp.StatusCode, truncate(string(body), 300))
		}
		return parseResponse(body, c.Prefix)
	}
	return Observation{}, lastErr
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
