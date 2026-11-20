// Package jsonpath reads values out of decoded JSON using the small path
// language that adapter files use.
//
// Paths are dotted field names. A "[]" after a name means "every element of
// this list", for example "data[].meta.n_ctx".
package jsonpath

import "strings"

// SplitArray splits a path at its first "[]". For "data[].meta.n_ctx" it
// returns ("data", "meta.n_ctx", true). If the path has no "[]" it returns
// ok = false.
func SplitArray(path string) (arrayPath, rest string, ok bool) {
	before, after, found := strings.Cut(path, "[]")
	if !found {
		return "", "", false
	}
	return before, strings.TrimPrefix(after, "."), true
}

// Get follows plain dotted fields (no "[]") and returns the value found.
func Get(v any, path string) (any, bool) {
	if path == "" {
		return v, true
	}
	for _, key := range strings.Split(path, ".") {
		obj, isObj := v.(map[string]any)
		if !isObj {
			return nil, false
		}
		next, exists := obj[key]
		if !exists {
			return nil, false
		}
		v = next
	}
	return v, true
}

// Has reports whether the path leads to a real (non-null) value.
// For a path with "[]", the list must be non-empty and its first element
// must contain the rest of the path.
func Has(v any, path string) bool {
	arrayPath, rest, hasArray := SplitArray(path)
	if !hasArray {
		got, ok := Get(v, path)
		return ok && got != nil
	}
	list, ok := Get(v, arrayPath)
	items, isList := list.([]any)
	if !ok || !isList || len(items) == 0 {
		return false
	}
	if rest == "" {
		return true
	}
	return Has(items[0], rest)
}

// IsList reports whether the path (plain dotted fields, no "[]") leads to a
// list. An empty list counts, so an engine with no models still matches.
func IsList(v any, path string) bool {
	got, ok := Get(v, path)
	_, isList := got.([]any)
	return ok && isList
}

// Collect returns every value the path leads to, across all list elements.
func Collect(v any, path string) []any {
	arrayPath, rest, hasArray := SplitArray(path)
	if !hasArray {
		if got, ok := Get(v, path); ok && got != nil {
			return []any{got}
		}
		return nil
	}
	list, ok := Get(v, arrayPath)
	items, isList := list.([]any)
	if !ok || !isList {
		return nil
	}
	var out []any
	for _, item := range items {
		if rest == "" {
			out = append(out, item)
			continue
		}
		out = append(out, Collect(item, rest)...)
	}
	return out
}
