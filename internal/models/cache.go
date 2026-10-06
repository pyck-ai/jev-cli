package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// DefaultTTL is how long a cached catalog counts as fresh.
const DefaultTTL = 24 * time.Hour

const (
	cacheSubdir = "jev-cli"
	cacheFile   = "models.json"
)

// Source says where Result.Catalog came from.
type Source string

const (
	SourceCache      Source = "cache"       // fresh cache, no network call
	SourceNetwork    Source = "network"     // just fetched
	SourceStaleCache Source = "stale-cache" // fetch failed; expired cache used
)

// Options tune Load. The zero value is the production default.
type Options struct {
	// Refresh forces a fetch even if the cache is fresh.
	Refresh bool
	// CacheDir overrides the directory holding models.json. Empty means
	// os.UserCacheDir()/jev-cli.
	CacheDir string
	// TTL overrides DefaultTTL when > 0.
	TTL time.Duration
	// Now overrides time.Now (tests).
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) ttl() time.Duration {
	if o.TTL > 0 {
		return o.TTL
	}
	return DefaultTTL
}

// CachePath returns the cache file path Load would use, or an error if
// no cache directory can be determined.
func (o Options) CachePath() (string, error) {
	dir := o.CacheDir
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, cacheSubdir)
	}
	return filepath.Join(dir, cacheFile), nil
}

// Result is what Load returns on success.
type Result struct {
	Catalog *Catalog
	Source  Source
	// Warning is non-fatal trouble: the fetch error behind a stale
	// catalog, and/or a cache read/write problem. Callers should print
	// it but carry on. Nil when everything went fine.
	Warning error
}

// Load returns the model catalog, from the cache when it is fresh and
// from g otherwise (see the package doc for the full failure behavior).
// The returned error is non-nil only when no catalog is available.
func Load(ctx context.Context, g Getter, opts Options) (*Result, error) {
	path, pathErr := opts.CachePath()
	var warns []error
	if pathErr != nil {
		warns = append(warns, fmt.Errorf("models: no cache directory, not caching: %w", pathErr))
	}

	var cached *Catalog
	if pathErr == nil {
		c, err := readCache(path)
		switch {
		case err == nil:
			cached = c
		case !errors.Is(err, os.ErrNotExist):
			warns = append(warns, fmt.Errorf("models: ignoring unreadable cache %s: %w", path, err))
		}
	}

	now := opts.now()
	if cached != nil && !opts.Refresh {
		age := now.Sub(cached.FetchedAt)
		if age >= 0 && age < opts.ttl() {
			return &Result{Catalog: cached, Source: SourceCache, Warning: errors.Join(warns...)}, nil
		}
	}

	fresh, err := Fetch(ctx, g)
	if err != nil {
		if cached != nil {
			warns = append(warns, fmt.Errorf("using stale model cache from %s: %w", cached.FetchedAt.Format(time.RFC3339), err))
			return &Result{Catalog: cached, Source: SourceStaleCache, Warning: errors.Join(warns...)}, nil
		}
		return nil, err
	}
	fresh.FetchedAt = now.UTC()
	if pathErr == nil {
		if werr := writeCache(path, fresh); werr != nil {
			warns = append(warns, fmt.Errorf("models: could not write cache %s: %w", path, werr))
		}
	}
	return &Result{Catalog: fresh, Source: SourceNetwork, Warning: errors.Join(warns...)}, nil
}

func readCache(path string) (*Catalog, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if len(c.Models) == 0 || c.FetchedAt.IsZero() {
		return nil, errors.New("empty or incomplete cache file")
	}
	return &c, nil
}

// writeCache writes c atomically: temp file in the target directory,
// then rename over the final path.
func writeCache(path string, c *Catalog) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, cacheFile+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
