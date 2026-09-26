package credentials

import (
	"os"
	"path/filepath"
	"testing"
)

// isolate points XDG_DATA_HOME at a fresh temp dir for the duration of the
// test, so Resolve()'s opencode-auth-store fallback can never touch the
// real ~/.local/share/opencode/auth.json, and clears OPENROUTER_API_KEY so
// tests start from a known "unset" state regardless of the ambient
// environment. Returns the temp dir.
func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv(EnvVar, "")
	return dir
}

// writeOpencodeAuth writes dir/opencode/auth.json with the given raw JSON
// content, creating parent directories as needed.
func writeOpencodeAuth(t *testing.T, dir, content string) {
	t.Helper()
	authDir := filepath.Join(dir, "opencode")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		t.Fatalf("creating opencode auth dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), []byte(content), 0o600); err != nil {
		t.Fatalf("writing opencode auth.json: %v", err)
	}
}

func TestResolve_EnvTakesPrecedenceOverFile(t *testing.T) {
	dir := isolate(t)
	writeOpencodeAuth(t, dir, `{"openrouter": {"type": "api", "key": "sk-or-from-file"}}`)
	t.Setenv(EnvVar, "sk-or-from-env")

	got, ok := Resolve()
	if !ok {
		t.Fatal("Resolve() ok = false, want true")
	}
	if got.Key != "sk-or-from-env" {
		t.Errorf("Key = %q, want the env var value (env must take precedence over the file)", got.Key)
	}
	if got.Source != "env" {
		t.Errorf(`Source = %q, want "env"`, got.Source)
	}
}

func TestResolve_FallsBackToOpencodeAuthFile(t *testing.T) {
	dir := isolate(t)
	writeOpencodeAuth(t, dir, `{
		"openrouter": {"type": "api", "key": "sk-or-from-file"},
		"anthropic": {"type": "oauth", "refresh": "x", "access": "y", "expires": 123}
	}`)

	got, ok := Resolve()
	if !ok {
		t.Fatal("Resolve() ok = false, want true (a usable key exists in the file)")
	}
	if got.Key != "sk-or-from-file" {
		t.Errorf("Key = %q, want %q", got.Key, "sk-or-from-file")
	}
	wantPath := filepath.Join(dir, "opencode", "auth.json")
	wantSource := "opencode auth store at " + wantPath
	if got.Source != wantSource {
		t.Errorf("Source = %q, want %q", got.Source, wantSource)
	}
}

func TestResolve_SkipsNonAPITypedOpenrouterEntry(t *testing.T) {
	dir := isolate(t)
	writeOpencodeAuth(t, dir, `{"openrouter": {"type": "oauth", "refresh": "r", "access": "a", "expires": 123}}`)

	_, ok := Resolve()
	if ok {
		t.Fatal("Resolve() ok = true, want false (an oauth-typed openrouter entry must not be usable)")
	}
}

func TestResolve_SkipsMissingOrMalformedFileAndFailsClosed(t *testing.T) {
	cases := map[string]struct {
		content   string // "" means: do not write the file at all
		writeFile bool
	}{
		"no file at all":                       {writeFile: false},
		"malformed json":                       {writeFile: true, content: `{not valid json`},
		"valid json, no openrouter key":        {writeFile: true, content: `{"anthropic": {"type": "oauth", "refresh": "x"}}`},
		"openrouter entry is not an object":    {writeFile: true, content: `{"openrouter": "sk-or-not-an-object"}`},
		"api type but empty key":               {writeFile: true, content: `{"openrouter": {"type": "api", "key": ""}}`},
		"api type but key field missing":       {writeFile: true, content: `{"openrouter": {"type": "api"}}`},
		"top level is a json array not object": {writeFile: true, content: `[1,2,3]`},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := isolate(t)
			if tc.writeFile {
				writeOpencodeAuth(t, dir, tc.content)
			}

			_, ok := Resolve()
			if ok {
				t.Fatalf("Resolve() ok = true, want false for case %q", name)
			}
		})
	}
}

func TestResolve_SkipsUnreadableFile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: file permissions are not enforced, this test would be unreliable")
	}
	dir := isolate(t)
	writeOpencodeAuth(t, dir, `{"openrouter": {"type": "api", "key": "sk-or-unreachable"}}`)
	path := filepath.Join(dir, "opencode", "auth.json")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) }) // let TempDir cleanup remove it afterwards

	_, ok := Resolve()
	if ok {
		t.Fatal("Resolve() ok = true, want false for a permission-denied auth.json")
	}
}
