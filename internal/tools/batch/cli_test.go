package batch

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/pyck-ai/jev-cli/internal/config"
	"github.com/pyck-ai/jev-cli/internal/registry"
)

func runCLI(t *testing.T, provider registry.DepsProvider, stdin string, args ...string) (string, int, error) {
	t.Helper()
	code := -1
	old := exitFunc
	exitFunc = func(c int) { code = c }
	t.Cleanup(func() { exitFunc = old })

	cmd := newCLICommand(provider, "desc")
	root := &cobra.Command{Use: "jev", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(cmd)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"batch"}, args...))
	err := root.Execute()
	return out.String(), code, err
}

func TestCLI_JSONInputDefaultsToJSONOutputAndExitsWithMax(t *testing.T) {
	f := newFake(t)
	deps, _ := newDeps(t, f, config.Default())
	provider := func() *registry.Deps { return deps }

	items := `{"items":[
	  {"id":"d","tool":"decide","input":{"decision":"pick:a","evidence":"e","priorities":"p","candidates":[{"id":"a","description":"A"},{"id":"b","description":"B"}]}},
	  {"id":"c","tool":"check","input":{"context":"uncertain","propositions":["x"]}}]}`

	out, code, err := runCLI(t, provider, "", "-j", items)
	if err != nil {
		t.Fatal(err)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1 (check wants review)", code)
	}
	var got BatchOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("default output is not JSON: %v\n%s", err, out)
	}
	if got.Summary.Items != 2 || got.Summary.ExitCode != 1 || got.Results[0].ID != "d" || got.Results[1].ID != "c" {
		t.Errorf("output = %+v", got)
	}
}

func TestCLI_ItemsFlagAndStdin(t *testing.T) {
	f := newFake(t)
	deps, _ := newDeps(t, f, config.Default())
	provider := func() *registry.Deps { return deps }
	arr := `[{"tool":"check","input":{"context":"fine","propositions":["x"]}}]`

	for name, args := range map[string][]string{
		"--items":  {"--items", arr},
		"-j stdin": {"-j", "-"},
	} {
		t.Run(name, func(t *testing.T) {
			stdin := ""
			if name == "-j stdin" {
				stdin = `{"items":` + arr + `}`
				pr, pw, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				_, _ = pw.WriteString(stdin)
				pw.Close()
				old := os.Stdin
				os.Stdin = pr // cliinput reads os.Stdin directly
				t.Cleanup(func() { os.Stdin = old; pr.Close() })
			}
			out, code, err := runCLI(t, provider, stdin, args...)
			if err != nil || code != 0 {
				t.Fatalf("err=%v code=%d out=%s", err, code, out)
			}
			if !strings.Contains(out, `"ok": 1`) {
				t.Errorf("out = %s", out)
			}
		})
	}
}

func TestCLI_MalformedBatchIsAnError(t *testing.T) {
	f := newFake(t)
	deps, _ := newDeps(t, f, config.Default())
	_, code, err := runCLI(t, func() *registry.Deps { return deps }, "", "-j", `{"items":[]}`)
	if err == nil || code != -1 {
		t.Errorf("err=%v code=%d, want error and no exit call", err, code)
	}
}
