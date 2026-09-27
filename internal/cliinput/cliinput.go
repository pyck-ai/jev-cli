// Package cliinput is the shared machinery every jev CLI subcommand uses
// to turn command-line flags (or a --json blob) into its tool's typed
// Input struct.
//
// Two input paths are always available on every subcommand, per the CLI
// design decisions:
//
//  1. Per-field flags: one flag per Input struct field, named by
//     converting the field's JSON tag from snake_case to kebab-case
//     (scale_min -> --scale-min). Scalar fields (string/int/float/bool)
//     take their literal value; structured fields (slices, maps, nested
//     structs) take a JSON-encoded string, e.g. --candidates
//     '[{"id":"a","text":"..."}]'.
//
//  2. --json / -j: the whole Input struct as one JSON object, either as
//     a literal argument value or as "-" to read from stdin. Combining
//     --json with any per-field flag is a hard error (exit 3): the tool
//     refuses to guess which representation wins.
//
// Required-field enforcement is deliberately NOT reimplemented here:
// every tool's core run function already validates its own input and
// returns precise error messages (see e.g. score.validateInput), and
// those errors surface as exit-3 hard errors through the CLI adapter.
// Duplicating per-field requiredness in this package would drift out of
// sync with the tools' own validators for no benefit.
//
// Field flags are registered reflectively from the Input type at Bind
// time and read back at Parse time, so this package holds no per-tool
// bookkeeping: adding a field to an Input struct is enough for its flag
// to exist (and the flag's usage text comes from the field's
// jsonschema description tag when present).
package cliinput

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// jsonFlagName / jsonFlagShorthand are the whole-input flags (see the
// package doc comment).
const (
	jsonFlagName      = "json"
	jsonFlagShorthand = "j"
	stdinValue        = "-"
)

// Binder holds the command a generic Input type's flags were registered
// on, so Parse can read them back at run time. Constructed by Bind.
type Binder[I any] struct {
	cmd *cobra.Command
}

// Bind registers one flag per exported field of I (which must be a
// struct) on cmd, plus the --json/-j whole-input flag. It returns the
// Binder whose Parse method the subcommand's RunE calls to produce an I.
//
// Field selection: exported fields with a `json` tag whose name is not
// "-" (fields without a json tag fall back to the lowercased field
// name, mirroring encoding/json). Flag naming: JSON tag snake_case ->
// kebab-case. Flag usage text: the field's `jsonschema` description tag
// when present, else a generic "<field> (JSON-encoded)" for structured
// fields.
//
// All flags are registered as plain strings (even for scalar numeric or
// boolean fields) so that Parse can distinguish "user passed --scale-min
// 2" from "user passed nothing" via Flags().Changed without zero-value
// ambiguity; type conversion happens at Parse time with a per-field
// error message naming the flag.
func Bind[I any](cmd *cobra.Command) *Binder[I] {
	b := &Binder[I]{cmd: cmd}

	t := reflect.TypeOf((*I)(nil)).Elem()
	if t.Kind() != reflect.Struct {
		// Every tool Input in this codebase is a struct; this is a
		// programming error in a tool's RegisterCLI, not a user error.
		panic(fmt.Sprintf("cliinput: Bind: %s is not a struct (tool Input must be a struct)", t))
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, ok := jsonFieldName(f)
		if !ok {
			continue
		}
		usage := f.Tag.Get("jsonschema")
		if usage == "" {
			usage = fmt.Sprintf("Value for %s.", name)
		}
		if isStructuredKind(f.Type.Kind()) {
			usage = strings.TrimSuffix(usage, ".") + " (JSON-encoded)."
		}
		cmd.Flags().String(kebab(name), "", usage)
	}

	cmd.Flags().StringP(jsonFlagName, jsonFlagShorthand, "",
		"Whole input as one JSON object (e.g. '{\"state\": ...}'), or '-' to read it from stdin. Mutually exclusive with every per-field flag.")
	return b
}

// Parse builds the I from whichever input path the user chose: the
// --json/-j flag (literal or stdin via '-'), or the per-field flags. See
// the package doc comment for the conflict rule.
func (b *Binder[I]) Parse() (I, error) {
	var zero I

	jsonFlag := b.cmd.Flags().Lookup(jsonFlagName)
	usingJSON := jsonFlag != nil && jsonFlag.Changed

	if usingJSON {
		// Conflict check: --json plus any per-field flag is a hard
		// error -- refuse to guess which representation wins.
		t := reflect.TypeOf((*I)(nil)).Elem()
		var conflicts []string
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, ok := jsonFieldName(f)
			if !ok {
				continue
			}
			if fl := b.cmd.Flags().Lookup(kebab(name)); fl != nil && fl.Changed {
				conflicts = append(conflicts, fl.Name)
			}
		}
		if len(conflicts) > 0 {
			slices.Sort(conflicts)
			return zero, fmt.Errorf("--json cannot be combined with per-field flag(s): --%s", strings.Join(conflicts, ", --"))
		}

		raw := jsonFlag.Value.String()
		if raw == stdinValue {
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return zero, fmt.Errorf("reading JSON input from stdin: %w", err)
			}
			raw = string(data)
		}
		var in I
		if err := json.Unmarshal([]byte(raw), &in); err != nil {
			return zero, fmt.Errorf("parsing --json input: %w", err)
		}
		return in, nil
	}

	// Per-field path: build a map from JSON tag to json.RawMessage per
	// field, then round-trip through JSON so structured fields (slices,
	// maps, nested structs -- whose flag values are themselves JSON) are
	// validated and decoded by encoding/json itself.
	t := reflect.TypeOf((*I)(nil)).Elem()
	m := make(map[string]json.RawMessage, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, ok := jsonFieldName(f)
		if !ok {
			continue
		}
		fl := b.cmd.Flags().Lookup(kebab(name))
		if fl == nil || !fl.Changed {
			continue
		}
		value := fl.Value.String()

		raw, err := fieldRawMessage(f, value)
		if err != nil {
			return zero, fmt.Errorf("--%s: %w", kebab(name), err)
		}
		m[name] = raw
	}
	encoded, err := json.Marshal(m)
	if err != nil {
		return zero, fmt.Errorf("assembling input: %w", err)
	}
	var in I
	if err := json.Unmarshal(encoded, &in); err != nil {
		return zero, fmt.Errorf("parsing input flags: %w", err)
	}
	return in, nil
}

// fieldRawMessage converts one flag's string value into the JSON
// representation appropriate for f's Go type. Scalars are converted by
// the obvious strconv route (so `--scale-min 2` becomes JSON `2`, and a
// string state never has to be JSON-quoted on the command line);
// structured kinds must already BE valid JSON text.
func fieldRawMessage(f reflect.StructField, value string) (json.RawMessage, error) {
	kind := f.Type.Kind()

	switch {
	case kind == reflect.String:
		return json.Marshal(value)
	case kind == reflect.Bool:
		v, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("must be true or false, got %q", value)
		}
		return json.Marshal(v)
	case isIntKind(kind):
		v, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("must be an integer, got %q", value)
		}
		return json.Marshal(v)
	case isUintKind(kind):
		v, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("must be an unsigned integer, got %q", value)
		}
		return json.Marshal(v)
	case kind == reflect.Float32 || kind == reflect.Float64:
		v, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("must be a number, got %q", value)
		}
		return json.Marshal(v)
	default:
		// Structured field: the flag value must already be valid JSON
		// of the field's type. Validate syntactic JSON-ness here for a
		// better error than a downstream unmarshal failure; type
		// mismatches against the field are still caught by the final
		// Unmarshal into I.
		if !json.Valid([]byte(value)) {
			return nil, fmt.Errorf("must be valid JSON, got %q", value)
		}
		return json.RawMessage(value), nil
	}
}

// jsonFieldName returns the JSON object key f marshals under, following
// encoding/json's rules closely enough for this codebase: the json tag's
// name when present and not "-", the lowercased field name otherwise.
// ok=false means the field is not part of the JSON shape at all
// (json:"-"), so no flag exists for it either.
func jsonFieldName(f reflect.StructField) (name string, ok bool) {
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	if tag == "" {
		return strings.ToLower(f.Name), true
	}
	name = strings.Split(tag, ",")[0]
	if name == "" {
		// e.g. json:",omitempty" -- nameless tag still means the
		// lowercased field name.
		return strings.ToLower(f.Name), true
	}
	return name, true
}

// kebab converts a snake_case JSON tag into its flag spelling.
func kebab(name string) string {
	return strings.ReplaceAll(name, "_", "-")
}

func isStructuredKind(k reflect.Kind) bool {
	switch k {
	case reflect.Slice, reflect.Array, reflect.Map, reflect.Struct, reflect.Ptr, reflect.Interface:
		return true
	}
	return false
}

func isIntKind(k reflect.Kind) bool {
	switch k {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return true
	}
	return false
}

func isUintKind(k reflect.Kind) bool {
	switch k {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return true
	}
	return false
}
