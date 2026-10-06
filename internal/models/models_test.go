package models

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGetter serves testdata/models.json (or an error) and counts calls.
type fakeGetter struct {
	err   error
	calls int
	path  string
}

func (f *fakeGetter) GetJSON(_ context.Context, path string, out any) error {
	f.calls++
	f.path = path
	if f.err != nil {
		return f.err
	}
	data, err := os.ReadFile(filepath.Join("testdata", "models.json"))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func TestFetch_ParsesFixture(t *testing.T) {
	g := &fakeGetter{}
	cat, err := Fetch(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	if g.path != "/models?output_modalities=decisions" {
		t.Errorf("path = %q", g.path)
	}
	if len(cat.Models) != 6 {
		t.Fatalf("got %d models, want 6", len(cat.Models))
	}
	m, ok := cat.Lookup("perplexity/pplx-decider-v1-27b")
	if !ok {
		t.Fatal("perplexity not found")
	}
	if m.ContextLength != 262144 || m.PromptPrice != 0.00000004 || m.CompletionPrice != 0 {
		t.Errorf("perplexity = %+v", m)
	}
	if got := m.PromptPricePerMillion(); got < 0.0399 || got > 0.0401 {
		t.Errorf("per million = %v", got)
	}
	if strings.Join(m.InputModalities, ",") != "text,image" || m.CreatedTime().Year() < 2026 {
		t.Errorf("modalities/created = %v %v", m.InputModalities, m.CreatedTime())
	}
	if m.CanonicalSlug != "perplexity/pplx-decider-v1-27b-20261001" {
		t.Errorf("canonical = %q", m.CanonicalSlug)
	}
	r, _ := cat.Lookup("respan/span-01")
	if r.ContextLength != 0 {
		t.Errorf("respan context = %d, want 0 (unknown)", r.ContextLength)
	}
}

func TestFetch_ErrorsAndEmpty(t *testing.T) {
	if _, err := Fetch(context.Background(), &fakeGetter{err: errors.New("boom")}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
	empty := getterFunc(func(_ context.Context, _ string, out any) error { return json.Unmarshal([]byte(`{"data":[]}`), out) })
	if _, err := Fetch(context.Background(), empty); err == nil {
		t.Error("empty list should be an error")
	}
}

type getterFunc func(ctx context.Context, path string, out any) error

func (f getterFunc) GetJSON(ctx context.Context, path string, out any) error {
	return f(ctx, path, out)
}

func TestLookup_AliasAndCanonical(t *testing.T) {
	cat, err := Fetch(context.Background(), &fakeGetter{})
	if err != nil {
		t.Fatal(err)
	}
	a, ok := cat.Lookup("~typesafe/jev-latest")
	if !ok || a.AliasTarget != "typesafe/jev-1.13" {
		t.Fatalf("alias = %+v ok=%v", a, ok)
	}
	if m, ok := cat.Lookup("typesafe/jev-1.13-20260917"); !ok || m.ID != "typesafe/jev-1.13" {
		t.Errorf("canonical lookup = %+v ok=%v", m, ok)
	}
	if r, ok := cat.Resolve("~typesafe/jev-latest"); !ok || r.ID != "typesafe/jev-1.13" {
		t.Errorf("Resolve = %+v ok=%v", r, ok)
	}
	if _, ok := cat.Lookup("nope/nope"); ok {
		t.Error("unknown slug found")
	}
	if _, ok := cat.Lookup(""); ok {
		t.Error("empty slug found")
	}
}

func TestLoad_FreshCacheTTLStaleRefresh(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	clock := now
	opts := Options{CacheDir: dir, Now: func() time.Time { return clock }}
	g := &fakeGetter{}
	ctx := context.Background()

	// 1. miss: network, cache written.
	r, err := Load(ctx, g, opts)
	if err != nil || r.Source != SourceNetwork || r.Warning != nil || g.calls != 1 {
		t.Fatalf("first: %+v err=%v calls=%d", r, err, g.calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "models.json")); err != nil {
		t.Fatalf("cache not written: %v", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp-*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}

	// 2. fresh (23h): cache, no call.
	clock = now.Add(23 * time.Hour)
	r, err = Load(ctx, g, opts)
	if err != nil || r.Source != SourceCache || g.calls != 1 || len(r.Catalog.Models) != 6 {
		t.Fatalf("fresh: %+v err=%v calls=%d", r, err, g.calls)
	}

	// 3. expired (25h): refetch.
	clock = now.Add(25 * time.Hour)
	r, err = Load(ctx, g, opts)
	if err != nil || r.Source != SourceNetwork || g.calls != 2 {
		t.Fatalf("expired: %+v err=%v calls=%d", r, err, g.calls)
	}

	// 4. Refresh forces a fetch even when fresh.
	ro := opts
	ro.Refresh = true
	r, err = Load(ctx, g, ro)
	if err != nil || r.Source != SourceNetwork || g.calls != 3 {
		t.Fatalf("refresh: %+v err=%v calls=%d", r, err, g.calls)
	}

	// 5. expired + fetch failure: stale catalog plus warning, no error.
	clock = clock.Add(48 * time.Hour)
	g.err = errors.New("network down")
	r, err = Load(ctx, g, opts)
	if err != nil || r.Source != SourceStaleCache || r.Warning == nil || !strings.Contains(r.Warning.Error(), "network down") || len(r.Catalog.Models) != 6 {
		t.Fatalf("stale: %+v err=%v", r, err)
	}

	// 6. Refresh + failure also falls back to stale.
	r, err = Load(ctx, g, Options{CacheDir: dir, Refresh: true, Now: opts.Now})
	if err != nil || r.Source != SourceStaleCache {
		t.Fatalf("refresh-stale: %+v err=%v", r, err)
	}
}

func TestLoad_NoCacheAndFetchFails(t *testing.T) {
	g := &fakeGetter{err: errors.New("network down")}
	if r, err := Load(context.Background(), g, Options{CacheDir: t.TempDir()}); err == nil || r != nil {
		t.Errorf("want error, got %+v %v", r, err)
	}
}

func TestLoad_CacheProblemsAreNotFatal(t *testing.T) {
	ctx := context.Background()

	t.Run("corrupt cache is a miss", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "models.json"), []byte("{not json"), 0o644)
		r, err := Load(ctx, &fakeGetter{}, Options{CacheDir: dir})
		if err != nil || r.Source != SourceNetwork || r.Warning == nil {
			t.Fatalf("%+v %v", r, err)
		}
		if _, err := readCache(filepath.Join(dir, "models.json")); err != nil {
			t.Errorf("cache not repaired: %v", err)
		}
	})

	t.Run("unwritable cache dir warns", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "afile")
		os.WriteFile(file, nil, 0o644)
		r, err := Load(ctx, &fakeGetter{}, Options{CacheDir: filepath.Join(file, "sub")})
		if err != nil || r.Source != SourceNetwork || r.Warning == nil {
			t.Fatalf("%+v %v", r, err)
		}
	})
}

func TestOptions_CachePathHonorsXDG(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "/tmp/xdgcache")
	p, err := Options{}.CachePath()
	if err != nil || p != "/tmp/xdgcache/jev-cli/models.json" {
		t.Errorf("CachePath = %q, %v", p, err)
	}
}

func runCmd(t *testing.T, g Getter, dir string, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewCommandWithOptions(func() (Getter, error) { return g, nil }, Options{CacheDir: dir})
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

func TestCommand_Text(t *testing.T) {
	out, _, err := runCmd(t, &fakeGetter{}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 7 {
		t.Fatalf("got %d lines:\n%s", len(lines), out)
	}
	for _, want := range []string{"SLUG", "CONTEXT", "$/1M IN", "INPUTS", "CREATED"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("header missing %q: %s", want, lines[0])
		}
	}
	// sorted by slug; "~" sorts after letters in ASCII
	var slugs []string
	for _, l := range lines[1:] {
		slugs = append(slugs, strings.Fields(l)[0])
	}
	want := []string{"liquid/d1", "perplexity/pplx-decider-v1-27b", "respan/span-01", "typesafe/jev-1.13", "~typesafe/jev-latest"}
	if len(slugs) != 6 || strings.Join(slugs[:2], ",") != "inception/mercury-decide:free,liquid/d1" || slugs[5] != want[4] {
		t.Errorf("slugs = %v", slugs)
	}
	for _, sub := range []string{"262144", "0.04", "text,image", "unknown", "~typesafe/jev-latest -> typesafe/jev-1.13", "2026-"} {
		if !strings.Contains(out, sub) {
			t.Errorf("output missing %q:\n%s", sub, out)
		}
	}
}

func TestCommand_JSONAndRefresh(t *testing.T) {
	g := &fakeGetter{}
	dir := t.TempDir()
	out, _, err := runCmd(t, g, dir, "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var doc ListOutput
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("bad json: %v\n%s", err, out)
	}
	if doc.Source != SourceNetwork || len(doc.Models) != 6 || doc.Models[0].ID != "inception/mercury-decide:free" {
		t.Errorf("doc = %+v", doc)
	}

	// second run uses the cache; --refresh does not.
	if _, _, err := runCmd(t, g, dir, "-o", "json"); err != nil || g.calls != 1 {
		t.Fatalf("cached run: err=%v calls=%d", err, g.calls)
	}
	if _, _, err := runCmd(t, g, dir, "--refresh"); err != nil || g.calls != 2 {
		t.Fatalf("refresh run: err=%v calls=%d", err, g.calls)
	}
}

func TestCommand_ProviderLazyAndErrors(t *testing.T) {
	called := false
	cmd := NewCommandWithOptions(func() (Getter, error) { called = true; return nil, errors.New("no key") }, Options{CacheDir: t.TempDir()})
	if called {
		t.Fatal("provider called at construction")
	}
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetArgs(nil)
	if err := cmd.Execute(); err == nil || err.Error() != "no key" || !called {
		t.Errorf("err = %v called=%v", err, called)
	}
}

func TestCommand_WarnsOnStale(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := runCmd(t, &fakeGetter{}, dir); err != nil {
		t.Fatal(err)
	}
	// Age the cache: rewrite with an old FetchedAt.
	c, _ := readCache(filepath.Join(dir, "models.json"))
	c.FetchedAt = time.Now().Add(-72 * time.Hour)
	if err := writeCache(filepath.Join(dir, "models.json"), c); err != nil {
		t.Fatal(err)
	}
	out, errOut, err := runCmd(t, &fakeGetter{err: errors.New("down")}, dir)
	if err != nil || !strings.Contains(errOut, "warning:") || !strings.Contains(out, "liquid/d1") {
		t.Errorf("err=%v stderr=%q", err, errOut)
	}
}
