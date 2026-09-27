// Package cliformat renders a tool's Output struct for the jev CLI: the
// shared, reflection-based human-readable "text" format used by default,
// and the JSON passthrough used with -o json.
//
// The text format (one renderer for every tool -- no per-tool render
// code to write or drift out of sync):
//
//   - Scalar fields: aligned `field: value` lines, in struct order.
//   - Map fields: a `field:` header, then one `  key: value` line per
//     entry, sorted by key (deterministic output).
//   - Slice-of-scalar fields: one `field: v1, v2, ...` line.
//   - Slice-of-struct fields: a `field:` header, then a text/tabwriter
//     table (header row from the element struct's JSON tags, one row per
//     element).
//   - Nested struct fields (and pointers to structs): a `field:` header
//     then the same rendering indented by two spaces; a nil pointer
//     renders as `field: null`.
//
// Field names are the JSON tag names (the same names the MCP tool's
// JSON output uses), so text and JSON output stay mentally aligned.
package cliformat

import (
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// AddOutputFlag registers the shared -o/--output flag on cmd. Values:
// "text" (default) or "json".
func AddOutputFlag(cmd *cobra.Command) {
	cmd.Flags().StringP("output", "o", "text", "Output format: text (default) or json")
}

// JSONRequested reports whether cmd's -o/--output flag asks for JSON.
func JSONRequested(cmd *cobra.Command) bool {
	return cmd.Flags().Lookup("output").Value.String() == "json"
}

// Emit writes out (which must be a struct, or a pointer to one) to w in
// the requested format: jsonMode=true renders json.MarshalIndent with
// two-space indentation (byte-compatible with the same struct marshaled
// anywhere else); jsonMode=false renders the human-readable text
// format described in the package doc comment.
func Emit(w io.Writer, out any, jsonMode bool) error {
	if jsonMode {
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("cliformat: marshaling output: %w", err)
		}
		_, err = fmt.Fprintln(w, string(data))
		return err
	}
	return Render(w, out)
}

// Render writes v in the human-readable text format described in the
// package doc comment.
func Render(w io.Writer, v any) error {
	rv := reflect.Indirect(reflect.ValueOf(v))
	if rv.Kind() != reflect.Struct {
		return fmt.Errorf("cliformat: Render: %s is not a struct", rv.Type())
	}
	r := &renderer{w: w}
	r.structFields(rv, 0)
	return nil
}

type renderer struct {
	w io.Writer
}

// structFields renders every renderable field of the struct rv at the
// given indent level. Scalar pairs are buffered so the `field: value`
// lines within one level share one alignment column (the format is
// aligned key-value lines, not a tabwriter table).
func (r *renderer) structFields(rv reflect.Value, indent int) {
	pad := strings.Repeat("  ", indent)

	type pair struct{ name, value string }
	var pairs []pair
	var blocks []func()

	t := rv.Type()
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, ok := jsonFieldName(f)
		if !ok {
			continue
		}
		fv := rv.Field(i)

		switch {
		case isScalarKind(fv.Kind()):
			pairs = append(pairs, pair{name, scalarString(fv)})

		case fv.Kind() == reflect.Map:
			blocks = append(blocks, func() { r.mapField(pad, name, fv) })

		case fv.Kind() == reflect.Slice || fv.Kind() == reflect.Array:
			elem := fv.Type().Elem()
			if elem.Kind() == reflect.Struct || (elem.Kind() == reflect.Pointer && elem.Elem().Kind() == reflect.Struct) {
				blocks = append(blocks, func() { r.tableField(pad, name, fv) })
			} else {
				pairs = append(pairs, pair{name, scalarString(fv)})
			}

		case fv.Kind() == reflect.Pointer:
			if fv.IsNil() {
				pairs = append(pairs, pair{name, "null"})
			} else if fv.Elem().Kind() == reflect.Struct {
				nested := fv.Elem()
				blocks = append(blocks, func() {
					fmt.Fprintf(r.w, "%s%s:\n", pad, name)
					r.structFields(nested, indent+1)
				})
			} else {
				pairs = append(pairs, pair{name, scalarString(fv.Elem())})
			}

		case fv.Kind() == reflect.Struct:
			blocks = append(blocks, func() {
				fmt.Fprintf(r.w, "%s%s:\n", pad, name)
				r.structFields(fv, indent+1)
			})

		default:
			pairs = append(pairs, pair{name, scalarString(fv)})
		}
	}

	if len(pairs) > 0 {
		width := 0
		for _, p := range pairs {
			if len(p.name)+1 > width {
				width = len(p.name) + 1
			}
		}
		for _, p := range pairs {
			fmt.Fprintf(r.w, "%s%-*s  %s\n", pad, width, p.name+":", p.value)
		}
	}
	for _, b := range blocks {
		b()
	}
}

func (r *renderer) mapField(pad, name string, fv reflect.Value) {
	fmt.Fprintf(r.w, "%s%s:\n", pad, name)
	keys := fv.MapKeys()
	sort.Slice(keys, func(i, j int) bool {
		return fmt.Sprint(keys[i].Interface()) < fmt.Sprint(keys[j].Interface())
	})
	type pair struct{ key, value string }
	var pairs []pair
	for _, k := range keys {
		elem := fv.MapIndex(k)
		if elem.Kind() == reflect.Interface || elem.Kind() == reflect.Pointer {
			elem = reflect.Indirect(elem)
		}
		pairs = append(pairs, pair{fmt.Sprint(k.Interface()), scalarString(elem)})
	}
	width := 0
	for _, p := range pairs {
		if len(p.key)+1 > width {
			width = len(p.key) + 1
		}
	}
	for _, p := range pairs {
		fmt.Fprintf(r.w, "%s  %-*s  %s\n", pad, width, p.key+":", p.value)
	}
}

func (r *renderer) tableField(pad, name string, fv reflect.Value) {
	fmt.Fprintf(r.w, "%s%s:\n", pad, name)
	tw := tabwriter.NewWriter(r.w, 0, 2, 2, ' ', 0)

	// Header: JSON tag names of the element struct's fields.
	elemType := fv.Type().Elem()
	if elemType.Kind() == reflect.Pointer {
		elemType = elemType.Elem()
	}
	var headers []string
	for i := range elemType.NumField() {
		f := elemType.Field(i)
		if !f.IsExported() {
			continue
		}
		name, ok := jsonFieldName(f)
		if !ok {
			continue
		}
		headers = append(headers, name)
	}
	if len(headers) > 0 {
		fmt.Fprintln(tw, pad+strings.Join(headers, "\t"))
	}

	for i := range fv.Len() {
		elem := reflect.Indirect(fv.Index(i))
		var cells []string
		for j := range elem.NumField() {
			f := elem.Type().Field(j)
			if !f.IsExported() {
				continue
			}
			if _, ok := jsonFieldName(f); !ok {
				continue
			}
			cells = append(cells, scalarString(reflect.Indirect(elem.Field(j))))
		}
		fmt.Fprintln(tw, pad+strings.Join(cells, "\t"))
	}
	tw.Flush()
}

// scalarString renders a value inline (used for top-level scalar
// fields, and recursively for any value nested inside a map entry or a
// table cell -- contexts that need a single-line rendering rather than
// the multi-line block layout structFields uses for a whole Output
// struct's own top-level nested-struct fields).
//
// Kinds handled: strings are quoted only when they need it for
// disambiguation; floats/bools/ints render plainly; nil pointers and
// nil interfaces render as "null" (a non-nil one recurses into its
// pointee); slices/arrays join their elements with ", "; maps render as
// sorted "key=value, key2=value2" pairs; structs render as
// "field=value, field2=value2" pairs using the same JSON field names
// the rest of this package uses, recursing on every field's value so a
// struct-valued map entry or table cell (e.g. jev_ask's per-question
// Answer, or a per-claim Probabilities map nested inside jev_verify's
// results table) never falls through to Go's raw %v struct/pointer
// dump -- which would print unexported internals and a live pointer
// address, a correctness bug (leaking an address) as well as an
// unreadable one.
func scalarString(v reflect.Value) string {
	if !v.IsValid() {
		return "null"
	}
	switch v.Kind() {
	case reflect.String:
		s := v.String()
		if strings.ContainsAny(s, " \t\n") {
			return strconv.Quote(s)
		}
		return s
	case reflect.Bool:
		return fmt.Sprintf("%v", v.Bool())
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return "null"
		}
		return scalarString(v.Elem())
	case reflect.Slice, reflect.Array:
		parts := make([]string, v.Len())
		for i := range v.Len() {
			parts[i] = scalarString(reflect.Indirect(v.Index(i)))
		}
		return strings.Join(parts, ", ")
	case reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool {
			return fmt.Sprint(keys[i].Interface()) < fmt.Sprint(keys[j].Interface())
		})
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s=%s", fmt.Sprint(k.Interface()), scalarString(v.MapIndex(k))))
		}
		return strings.Join(parts, ", ")
	case reflect.Struct:
		t := v.Type()
		var parts []string
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, ok := jsonFieldName(f)
			if !ok {
				continue
			}
			parts = append(parts, fmt.Sprintf("%s=%s", name, scalarString(v.Field(i))))
		}
		return strings.Join(parts, ", ")
	default:
		return fmt.Sprint(v.Interface())
	}
}

func isScalarKind(k reflect.Kind) bool {
	switch k {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

// jsonFieldName mirrors encoding/json's name rule (and cliinput's copy
// of it): the json tag's name when present and not "-", lowercased
// field name otherwise, ok=false for json:"-".
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
		return strings.ToLower(f.Name), true
	}
	return name, true
}
