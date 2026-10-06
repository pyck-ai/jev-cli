// Package route decides how jev-cli reaches SystemOne: through the local
// LiteLLM proxy, or directly at OpenRouter.
//
// The proxy forwards http://<base>/openrouter/api/v1/* to
// https://openrouter.ai/api/v1/* unchanged (LiteLLM swaps the
// Authorization header for its own OpenRouter key), so the SystemOne
// request body and headers are identical on both routes; only the URL and
// the bearer credential differ. Usage/cost fields come back unchanged, so
// budget accounting is route-independent (verified live 2026-10-04).
//
// # Selection (JEV_CLI_ROUTE=auto, the default)
//
//  1. Proxy candidate: PYCKLLM_API_KEY (else config proxy_api_key) is set.
//     No candidate means direct, with no probe.
//  2. One probe per process: GET {base}/openrouter/api/v1/key with the
//     virtual key, ~1.5 s timeout. 200 selects the proxy; anything else
//     (refused, timeout, 401/403/404, ...) selects direct.
//  3. Direct uses credentials.Resolve (OPENROUTER_API_KEY, then opencode's
//     auth store), unchanged. If direct has no key either, the error names
//     both routes.
//
// base comes from PYCKLLM_BASE_URL (else config proxy_base_url, else
// http://127.0.0.1:53986); a trailing "/" and "/v1" are stripped.
//
// When the proxy is selected and a direct key exists, the direct route is
// handed to the openrouter.Client as a fallback: a connection-level proxy
// failure mid-session retries that call once direct and marks the proxy
// down for the rest of the process (see openrouter.Client). HTTP error
// responses from the proxy are real answers and are never retried direct.
//
// JEV_CLI_ROUTE=proxy fails if the proxy is unusable (no fallback);
// JEV_CLI_ROUTE=direct never probes or uses the proxy.
//
// Keys are never logged or placed in error text; Route.CredentialSource
// names where a key came from, never the key.
package route

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/credentials"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
)

// DefaultProxyBase is the proxy base root used when neither
// PYCKLLM_BASE_URL nor the config file give one.
const DefaultProxyBase = "http://127.0.0.1:53986"

// ProbeTimeout bounds the single detection probe.
const ProbeTimeout = 1500 * time.Millisecond

// Mode values for JEV_CLI_ROUTE.
const (
	ModeAuto   = "auto"
	ModeProxy  = "proxy"
	ModeDirect = "direct"
)

// Options carries Resolve's dependencies; zero values select production
// behavior. Tests override Direct and HTTPClient.
type Options struct {
	Config       config.Config
	HTTPClient   *http.Client                      // probe client; default has ProbeTimeout
	Direct       func() (credentials.Result, bool) // default credentials.Resolve
	ProbeTimeout time.Duration                     // default ProbeTimeout
}

// Result is the outcome of Resolve: the route to use first and an optional
// direct fallback for mid-session proxy failure.
type Result struct {
	Primary  openrouter.Route
	Fallback *openrouter.Route
}

// NormalizeBase trims whitespace, trailing slashes and a trailing "/v1"
// from a proxy base URL, so both "http://h:1" and "http://h:1/v1" work.
func NormalizeBase(b string) string {
	b = strings.TrimRight(strings.TrimSpace(b), "/")
	b = strings.TrimSuffix(b, "/v1")
	return strings.TrimRight(b, "/")
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Mode returns the validated JEV_CLI_ROUTE value (default auto).
func Mode() (string, error) {
	m := strings.ToLower(strings.TrimSpace(os.Getenv(config.EnvRoute)))
	switch m {
	case "":
		return ModeAuto, nil
	case ModeAuto, ModeProxy, ModeDirect:
		return m, nil
	}
	return "", fmt.Errorf("%s=%q: want auto, proxy or direct", config.EnvRoute, m)
}

// Resolve picks the route per the package doc comment. It performs at most
// one network call (the probe), and none unless a proxy candidate exists
// and the mode is not direct.
func Resolve(ctx context.Context, opts Options) (Result, error) {
	mode, err := Mode()
	if err != nil {
		return Result{}, err
	}
	directFn := opts.Direct
	if directFn == nil {
		directFn = credentials.Resolve
	}

	var direct *openrouter.Route
	if cred, ok := directFn(); ok {
		direct = &openrouter.Route{
			Name:             openrouter.RouteDirect,
			Endpoint:         openrouter.Endpoint,
			APIKey:           cred.Key,
			BaseURL:          openrouter.BaseURL,
			CredentialSource: sourceLabel(cred.Source),
		}
	}
	noDirect := fmt.Sprintf("no direct OpenRouter key (set %s, or an \"openrouter\" api credential in opencode's auth store)", credentials.EnvVar)

	useDirect := func(why string) (Result, error) {
		if direct == nil {
			return Result{}, fmt.Errorf("no usable route: proxy: %s; direct: %s", why, noDirect)
		}
		r := *direct
		r.Why = why
		return Result{Primary: r}, nil
	}

	if mode == ModeDirect {
		if direct == nil {
			return Result{}, fmt.Errorf("%s=direct but %s", config.EnvRoute, noDirect)
		}
		return useDirect(config.EnvRoute + "=direct")
	}

	// Proxy candidate.
	key, keySrc := os.Getenv(config.EnvProxyAPIKey), "env "+config.EnvProxyAPIKey
	if key == "" {
		key, keySrc = opts.Config.ProxyAPIKey, "config proxy_api_key"
	}
	if key == "" {
		why := config.EnvProxyAPIKey + " not set (no proxy candidate)"
		if mode == ModeProxy {
			return Result{}, fmt.Errorf("%s=proxy but %s", config.EnvRoute, why)
		}
		return useDirect(why)
	}
	base := NormalizeBase(firstNonEmpty(os.Getenv(config.EnvProxyBaseURL), firstNonEmpty(opts.Config.ProxyBaseURL, DefaultProxyBase)))
	apiBase := base + "/openrouter/api/v1"

	if perr := probe(ctx, opts, apiBase, key); perr != nil {
		why := "proxy probe failed: " + perr.Error()
		if mode == ModeProxy {
			return Result{}, fmt.Errorf("%s=proxy but the proxy at %s is unusable: %s", config.EnvRoute, apiBase, perr.Error())
		}
		return useDirect(why)
	}

	primary := openrouter.Route{
		Name:             openrouter.RouteProxy,
		Endpoint:         apiBase + "/systemone",
		APIKey:           key,
		BaseURL:          apiBase,
		CredentialSource: keySrc,
		Why:              "proxy probe ok (GET /key 200)",
	}
	if mode == ModeProxy {
		primary.Why = config.EnvRoute + "=proxy; probe ok (GET /key 200)"
		return Result{Primary: primary}, nil
	}
	return Result{Primary: primary, Fallback: direct}, nil
}

// sourceLabel turns credentials.Result.Source into the doctor label.
func sourceLabel(src string) string {
	if src == "env" {
		return "env " + credentials.EnvVar
	}
	return src
}

// probe issues the single detection request. The returned error text never
// includes the key (the key only appears in the Authorization header).
func probe(ctx context.Context, opts Options, apiBase, key string) error {
	timeout := opts.ProbeTimeout
	if timeout <= 0 {
		timeout = ProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+"/key", nil)
	if err != nil {
		return fmt.Errorf("building probe request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s/key: %w", apiBase, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s/key: HTTP %d", apiBase, resp.StatusCode)
	}
	return nil
}
