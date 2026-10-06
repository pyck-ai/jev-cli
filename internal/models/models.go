// Package models is jev-cli's catalog of SystemOne decision models. It
// holds NO model list and NO per-model limits of its own: everything
// (slugs, context length, prices, input modalities) comes from
// OpenRouter's model listing, `GET <apiBase>/models?output_modalities=decisions`,
// which works both directly and through the LiteLLM proxy (the Client's
// active route decides; see internal/openrouter and internal/route).
//
// # Cache
//
// Load keeps the last successful listing in a JSON file at
//
//	<os.UserCacheDir()>/jev-cli/models.json
//
// UserCacheDir honors XDG_CACHE_HOME and falls back to $HOME/.cache; in
// the Docker image HOME=/tmp, so the file lives at /tmp/.cache/jev-cli/.
// Options.CacheDir overrides the directory (tests, read-only homes).
// The file is written atomically (temp file in the same directory, then
// rename), so a concurrent reader never sees a partial file.
//
// # Freshness and failure behavior
//
//   - A cache younger than Options.TTL (default 24h, see DefaultTTL) is
//     returned as is, with no network call (Source == SourceCache).
//   - Otherwise, or with Options.Refresh, the listing is fetched and the
//     cache rewritten (SourceNetwork).
//   - If that fetch fails and ANY cache exists (even an expired one),
//     the stale catalog is returned with a non-fatal Result.Warning
//     (SourceStaleCache).
//   - Load returns an error only when there is no usable catalog at all
//     (no cache and the fetch failed).
//   - Cache problems (unresolvable cache dir, unreadable/corrupt file,
//     failed write) are never fatal: a bad cache counts as a miss, and a
//     failed write is reported in Result.Warning while the freshly
//     fetched catalog is still returned.
//
// # Guard and learned limits
//
// Guard is an openrouter.Hook (register it with Client.AddHook) that
// refuses requests known to fail, before anything is sent:
//
//   - Context length: the input is estimated as (JSON size of the state
//     plus every question's instructions and criteria) / 4 tokens; above
//     the model's catalog ContextLength the call fails with a
//     *PreflightError (Kind PreflightContext). ContextLength 0 (unknown)
//     skips the check.
//   - Unknown model: a slug absent from the catalog gets one stderr
//     warning per slug and is sent anyway; the provider's answer rules.
//     An unavailable catalog likewise warns once and sends everything.
//   - Learned limits: AfterAsk records an HTTP 400 for a model that IS in
//     the catalog in limits.json, next to models.json, for the catalog
//     TTL. An entry holds the resolved model slug, the provider's message
//     and a request-shape Signature (question types, state kind and
//     top-level keys, max choice-option count, log2 size bucket of the
//     state, question count), never request content. A later request with
//     the same model and signature fails early (Kind PreflightLearned)
//     quoting that message. A 400 for a model not in the catalog ("does
//     not exist") is not learned. Corrupt or unwritable files are never
//     fatal. `jev models --refresh` deletes limits.json (ClearLimits).
//
// # Wiring the `jev models` command
//
// NewCommand returns a ready cobra command; its provider is called lazily
// (only in RunE), so registering it costs nothing at startup:
//
//	root.AddCommand(models.NewCommand(func() (models.Getter, error) {
//		return buildOpenRouterClient() // any *openrouter.Client
//	}))
package models

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// listPath is the listing request, relative to the route's API base.
const listPath = "/models?output_modalities=decisions"

// Getter is the slice of *openrouter.Client the catalog needs: an
// authenticated GET relative to the active route's API base, decoding
// the JSON response into out.
type Getter interface {
	GetJSON(ctx context.Context, path string, out any) error
}

// Model is one SystemOne decision model, as listed by the API.
type Model struct {
	// ID is the slug callers pass as the model (e.g. "typesafe/jev-1.13",
	// or the alias "~typesafe/jev-latest").
	ID string `json:"id"`
	// CanonicalSlug is the dated/stable slug (may equal ID).
	CanonicalSlug string `json:"canonical_slug,omitempty"`
	Name          string `json:"name,omitempty"`
	Description   string `json:"description,omitempty"`
	// Created is a unix timestamp (seconds); 0 if absent.
	Created int64 `json:"created,omitempty"`
	// ContextLength is in tokens; 0 means the API does not say.
	ContextLength int `json:"context_length"`
	// InputModalities, e.g. ["text"] or ["text","image"].
	InputModalities []string `json:"input_modalities,omitempty"`
	// PromptPrice and CompletionPrice are USD per token, parsed from the
	// API's decimal strings. An empty or unparseable string yields 0.
	PromptPrice     float64 `json:"prompt_price"`
	CompletionPrice float64 `json:"completion_price"`
	// AliasTarget is the slug an alias model points at ("" for a
	// regular model).
	AliasTarget string `json:"alias_target,omitempty"`
}

// PromptPricePerMillion is the input price in USD per 1M tokens.
func (m Model) PromptPricePerMillion() float64 { return m.PromptPrice * 1e6 }

// CreatedTime is Created as a UTC time (zero Time if Created is 0).
func (m Model) CreatedTime() time.Time {
	if m.Created == 0 {
		return time.Time{}
	}
	return time.Unix(m.Created, 0).UTC()
}

// Catalog is one fetched model listing.
type Catalog struct {
	FetchedAt time.Time `json:"fetched_at"`
	Models    []Model   `json:"models"`
}

// Lookup finds a model by slug: first by exact ID (so an alias such as
// "~typesafe/jev-latest" matches its own entry), then by CanonicalSlug.
func (c *Catalog) Lookup(slug string) (Model, bool) {
	if c == nil || slug == "" {
		return Model{}, false
	}
	for _, m := range c.Models {
		if m.ID == slug {
			return m, true
		}
	}
	for _, m := range c.Models {
		if m.CanonicalSlug == slug {
			return m, true
		}
	}
	return Model{}, false
}

// Resolve is Lookup that follows an alias to its target model (one hop).
// If the alias target is not itself listed, the alias entry is returned.
func (c *Catalog) Resolve(slug string) (Model, bool) {
	m, ok := c.Lookup(slug)
	if !ok {
		return Model{}, false
	}
	if m.AliasTarget != "" {
		if t, ok := c.Lookup(m.AliasTarget); ok {
			return t, true
		}
	}
	return m, true
}

// wire types: the subset of the OpenRouter listing we use.
type wireList struct {
	Data []wireModel `json:"data"`
}

type wireModel struct {
	ID            string `json:"id"`
	CanonicalSlug string `json:"canonical_slug"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	Created       int64  `json:"created"`
	ContextLength int    `json:"context_length"`
	AliasTarget   *struct {
		Slug string `json:"slug"`
	} `json:"alias_target"`
	Architecture struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	Pricing struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
}

func parsePrice(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

// Fetch downloads the current decision-model listing. It does not touch
// the cache (see Load). An empty listing is an error, so a bad response
// can never overwrite a good cache.
func Fetch(ctx context.Context, g Getter) (*Catalog, error) {
	var list wireList
	if err := g.GetJSON(ctx, listPath, &list); err != nil {
		return nil, fmt.Errorf("models: fetching decision model list: %w", err)
	}
	cat := &Catalog{FetchedAt: time.Now().UTC()}
	for _, w := range list.Data {
		if w.ID == "" {
			continue
		}
		m := Model{
			ID:              w.ID,
			CanonicalSlug:   w.CanonicalSlug,
			Name:            w.Name,
			Description:     w.Description,
			Created:         w.Created,
			ContextLength:   w.ContextLength,
			InputModalities: w.Architecture.InputModalities,
			PromptPrice:     parsePrice(w.Pricing.Prompt),
			CompletionPrice: parsePrice(w.Pricing.Completion),
		}
		if w.AliasTarget != nil {
			m.AliasTarget = w.AliasTarget.Slug
		}
		cat.Models = append(cat.Models, m)
	}
	if len(cat.Models) == 0 {
		return nil, fmt.Errorf("models: decision model list is empty")
	}
	return cat, nil
}
