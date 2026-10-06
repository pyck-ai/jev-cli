package models

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const limitsFile = "limits.json"

// LearnedLimit is a request shape a model's provider rejected with an
// HTTP 400, remembered so an equivalent request is not sent (and billed
// or rate-limited) again. It is learned from the API's own answer, never
// from a built-in table. Signature describes the request SHAPE only (see
// Signature); Message is the provider's original error text.
type LearnedLimit struct {
	Model     string    `json:"model"` // resolved canonical catalog slug
	Signature string    `json:"signature"`
	Message   string    `json:"message"`
	LearnedAt time.Time `json:"learned_at"`
}

// Expires is when the entry stops applying, given the TTL.
func (l LearnedLimit) Expires(ttl time.Duration) time.Time { return l.LearnedAt.Add(ttl) }

type limitsDoc struct {
	Limits []LearnedLimit `json:"limits"`
}

// LimitsPath returns the learned-limits file path, next to the catalog
// cache (<cache dir>/jev-cli/limits.json, or opts.CacheDir/limits.json).
func (o Options) LimitsPath() (string, error) {
	p, err := o.CachePath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), limitsFile), nil
}

// ClearLimits deletes the learned limits (`jev models --refresh`). A
// missing file is not an error.
func ClearLimits(opts Options) error {
	p, err := opts.LimitsPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// readLimits returns the stored limits, dropping expired ones. A missing
// or corrupt file yields none.
func readLimits(path string, now time.Time, ttl time.Duration) []LearnedLimit {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc limitsDoc
	if json.Unmarshal(data, &doc) != nil {
		return nil
	}
	var live []LearnedLimit
	for _, l := range doc.Limits {
		if now.Before(l.Expires(ttl)) {
			live = append(live, l)
		}
	}
	return live
}

// writeLimits writes atomically: temp file in the target directory, then
// rename over the final path.
func writeLimits(path string, limits []LearnedLimit) error {
	data, err := json.MarshalIndent(limitsDoc{Limits: limits}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, limitsFile+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}
