package route

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/credentials"
	"github.com/pyck-ai/jev-cli/internal/openrouter"
)

const (
	vkey = "virtual-key-SECRET"
	dkey = "direct-key-SECRET"
)

func withDirect() func() (credentials.Result, bool) {
	return func() (credentials.Result, bool) { return credentials.Result{Key: dkey, Source: "env"}, true }
}

func noDirect() (credentials.Result, bool) { return credentials.Result{}, false }

// proxyServer answers GET /openrouter/api/v1/key with status, counting probes
// and recording the Authorization header.
func proxyServer(t *testing.T, status int) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	t.Helper()
	var n atomic.Int32
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openrouter/api/v1/key" || r.Method != http.MethodGet {
			w.WriteHeader(404)
			return
		}
		n.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &n, &auth
}

func setEnv(t *testing.T, key, base, mode string) {
	t.Helper()
	t.Setenv(config.EnvProxyAPIKey, key)
	t.Setenv(config.EnvProxyBaseURL, base)
	t.Setenv(config.EnvRoute, mode)
}

func TestNormalizeBase(t *testing.T) {
	for in, want := range map[string]string{
		"http://h:1":      "http://h:1",
		"http://h:1/":     "http://h:1",
		"http://h:1/v1":   "http://h:1",
		"http://h:1/v1/":  "http://h:1",
		" http://h:1/v1 ": "http://h:1",
	} {
		if got := NormalizeBase(in); got != want {
			t.Errorf("NormalizeBase(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestResolve_NoCandidate_DirectWithoutProbe(t *testing.T) {
	srv, n, _ := proxyServer(t, 200)
	setEnv(t, "", srv.URL, "")
	res, err := Resolve(context.Background(), Options{Direct: withDirect()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Primary.Name != openrouter.RouteDirect || res.Primary.APIKey != dkey {
		t.Errorf("got %+v, want direct", res.Primary)
	}
	if n.Load() != 0 {
		t.Errorf("probed %d times with no candidate, want 0", n.Load())
	}
}

func TestResolve_Probe200_Proxy(t *testing.T) {
	srv, n, auth := proxyServer(t, 200)
	setEnv(t, vkey, srv.URL+"/v1", "")
	res, err := Resolve(context.Background(), Options{Direct: withDirect()})
	if err != nil {
		t.Fatal(err)
	}
	p := res.Primary
	if p.Name != openrouter.RouteProxy || p.APIKey != vkey {
		t.Fatalf("got %+v, want proxy", p)
	}
	if p.BaseURL != srv.URL+"/openrouter/api/v1" || p.Endpoint != srv.URL+"/openrouter/api/v1/systemone" {
		t.Errorf("base/endpoint = %q / %q (the /v1 suffix must be stripped)", p.BaseURL, p.Endpoint)
	}
	if p.CredentialSource != "env PYCKLLM_API_KEY" {
		t.Errorf("CredentialSource = %q", p.CredentialSource)
	}
	if res.Fallback == nil || res.Fallback.Name != openrouter.RouteDirect {
		t.Errorf("want direct fallback, got %+v", res.Fallback)
	}
	if n.Load() != 1 || auth.Load() != "Bearer "+vkey {
		t.Errorf("probes=%d auth ok=%v", n.Load(), auth.Load() == "Bearer "+vkey)
	}
}

func TestResolve_ConfigFallbackAndEnvWins(t *testing.T) {
	srv, _, auth := proxyServer(t, 200)
	setEnv(t, "", "", "")
	cfg := config.Config{ProxyAPIKey: "cfgkey", ProxyBaseURL: srv.URL}
	res, err := Resolve(context.Background(), Options{Config: cfg, Direct: withDirect()})
	if err != nil || res.Primary.Name != openrouter.RouteProxy || res.Primary.CredentialSource != "config proxy_api_key" {
		t.Fatalf("config fallback: %+v, %v", res.Primary, err)
	}
	if auth.Load() != "Bearer cfgkey" {
		t.Errorf("probe did not use the config key")
	}
	setEnv(t, "envkey", srv.URL, "")
	res, err = Resolve(context.Background(), Options{Config: config.Config{ProxyAPIKey: "cfgkey", ProxyBaseURL: "http://127.0.0.1:1"}, Direct: withDirect()})
	if err != nil || res.Primary.APIKey != "envkey" || res.Primary.Name != openrouter.RouteProxy {
		t.Fatalf("env must win over config: %+v, %v", res.Primary, err)
	}
}

func TestResolve_Probe401_FallsBackDirect(t *testing.T) {
	for _, status := range []int{401, 403, 404, 500} {
		srv, _, _ := proxyServer(t, status)
		setEnv(t, vkey, srv.URL, "")
		res, err := Resolve(context.Background(), Options{Direct: withDirect()})
		if err != nil {
			t.Fatal(err)
		}
		if res.Primary.Name != openrouter.RouteDirect || res.Fallback != nil {
			t.Errorf("status %d: got %+v, want direct", status, res.Primary)
		}
		if !strings.Contains(res.Primary.Why, "HTTP") {
			t.Errorf("why = %q should carry the probe result", res.Primary.Why)
		}
	}
}

func TestResolve_ProbeRefused_FallsBackDirect(t *testing.T) {
	setEnv(t, vkey, "http://127.0.0.1:1", "")
	res, err := Resolve(context.Background(), Options{Direct: withDirect()})
	if err != nil || res.Primary.Name != openrouter.RouteDirect {
		t.Fatalf("got %+v, %v", res.Primary, err)
	}
	if strings.Contains(res.Primary.Why, vkey) {
		t.Errorf("why leaks the key: %q", res.Primary.Why)
	}
}

func TestResolve_NoRoute_ErrorNamesBoth(t *testing.T) {
	setEnv(t, vkey, "http://127.0.0.1:1", "")
	_, err := Resolve(context.Background(), Options{Direct: noDirect})
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"proxy", "direct", "OPENROUTER_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if strings.Contains(err.Error(), vkey) {
		t.Error("error leaks the key")
	}
}

func TestResolve_OverrideProxy(t *testing.T) {
	srv, _, _ := proxyServer(t, 200)
	setEnv(t, vkey, srv.URL, "proxy")
	res, err := Resolve(context.Background(), Options{Direct: withDirect()})
	if err != nil || res.Primary.Name != openrouter.RouteProxy || res.Fallback != nil {
		t.Fatalf("proxy mode: want proxy and no fallback, got %+v fb=%v err=%v", res.Primary, res.Fallback, err)
	}
	// Unusable proxy: hard error even though a direct key exists.
	setEnv(t, vkey, "http://127.0.0.1:1", "proxy")
	if _, err := Resolve(context.Background(), Options{Direct: withDirect()}); err == nil || !strings.Contains(err.Error(), "unusable") {
		t.Errorf("want unusable-proxy error, got %v", err)
	}
	// No candidate.
	setEnv(t, "", srv.URL, "proxy")
	if _, err := Resolve(context.Background(), Options{Direct: withDirect()}); err == nil {
		t.Error("proxy mode without a key must fail")
	}
}

func TestResolve_OverrideDirect_NoProbe(t *testing.T) {
	srv, n, _ := proxyServer(t, 200)
	setEnv(t, vkey, srv.URL, "direct")
	res, err := Resolve(context.Background(), Options{Direct: withDirect()})
	if err != nil || res.Primary.Name != openrouter.RouteDirect || res.Fallback != nil {
		t.Fatalf("got %+v, %v", res.Primary, err)
	}
	if n.Load() != 0 {
		t.Error("direct mode must not probe")
	}
	if _, err := Resolve(context.Background(), Options{Direct: noDirect}); err == nil {
		t.Error("direct mode without a direct key must fail")
	}
}

func TestResolve_BadMode(t *testing.T) {
	setEnv(t, "", "", "sideways")
	if _, err := Resolve(context.Background(), Options{Direct: withDirect()}); err == nil {
		t.Error("want error for invalid JEV_CLI_ROUTE")
	}
}
