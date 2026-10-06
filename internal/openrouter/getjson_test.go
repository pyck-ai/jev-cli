package openrouter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClient_GetJSON_SendsBearerAndDecodes(t *testing.T) {
	var gotAuth, gotPath, gotQuery, gotMethod string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotQuery, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.URL.RawQuery, r.Method
		w.Write([]byte(`{"data":[{"id":"a/b"}]}`))
	}))
	defer srv.Close()

	c := NewClientWithRoutes(Route{Name: RouteDirect, Endpoint: srv.URL + "/systemone", APIKey: "sk-secret", BaseURL: srv.URL + "/api/v1"}, nil, RetryPolicy{MaxAttempts: 1})
	var out struct {
		Data []struct{ ID string } `json:"data"`
	}
	if err := c.GetJSON(context.Background(), "/models?output_modalities=decisions", &out); err != nil {
		t.Fatalf("GetJSON: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/api/v1/models" || gotQuery != "output_modalities=decisions" {
		t.Errorf("request = %s %s?%s", gotMethod, gotPath, gotQuery)
	}
	if gotAuth != "Bearer sk-secret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if len(out.Data) != 1 || out.Data[0].ID != "a/b" {
		t.Errorf("out = %+v", out)
	}
}

func TestClient_GetJSON_APIErrorAndNoKeyLeak(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"code":401,"message":"bad key"}}`))
	}))
	defer srv.Close()

	c := NewClientWithRoutes(Route{Name: RouteDirect, APIKey: "sk-secret", BaseURL: srv.URL}, nil, RetryPolicy{MaxAttempts: 1})
	var out map[string]any
	err := c.GetJSON(context.Background(), "models", &out)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 || apiErr.Message != "bad key" {
		t.Fatalf("err = %v, want *APIError 401 bad key", err)
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Errorf("error leaks key: %v", err)
	}
}

func TestClient_GetJSON_FailsOverFromProxyToDirect(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // connection refused

	var gotAuth string
	direct := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer direct.Close()

	c := NewClientWithRoutes(
		Route{Name: RouteProxy, APIKey: "proxy-key", BaseURL: deadURL + "/openrouter/api/v1"},
		&Route{Name: RouteDirect, APIKey: "direct-key", BaseURL: direct.URL},
		RetryPolicy{MaxAttempts: 1})
	var out struct{ OK bool }
	if err := c.GetJSON(context.Background(), "/models", &out); err != nil || !out.OK {
		t.Fatalf("GetJSON = %v, out=%+v", err, out)
	}
	if gotAuth != "Bearer direct-key" || c.RouteInfo().Route != RouteDirect {
		t.Errorf("auth=%q route=%q", gotAuth, c.RouteInfo().Route)
	}
}
