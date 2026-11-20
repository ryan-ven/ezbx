package jsonpath

import (
	"encoding/json"
	"reflect"
	"testing"
)

func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSplitArray(t *testing.T) {
	arr, rest, ok := SplitArray("data[].meta.n_ctx")
	if arr != "data" || rest != "meta.n_ctx" || !ok {
		t.Fatalf("got %q %q %v", arr, rest, ok)
	}
	if _, _, ok := SplitArray("max_context"); ok {
		t.Fatal("a path without [] must report ok = false")
	}
}

func TestHas(t *testing.T) {
	doc := decode(t, `{"max_context": 10, "nothing": null, "data": [{"id": "a", "status": {"value": "loaded"}}], "empty": []}`)

	cases := map[string]bool{
		"max_context":          true,
		"nothing":              false, // null counts as missing
		"missing":              false,
		"data[]":               true,
		"data[].id":            true,
		"data[].status.value":  true,
		"data[].nope":          false,
		"empty[].id":           false, // an empty list has no first element
		"max_context.too.deep": false,
	}
	for path, want := range cases {
		if got := Has(doc, path); got != want {
			t.Errorf("Has(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestCollect(t *testing.T) {
	doc := decode(t, `{"running": [{"url": "http://a"}, {"url": "http://b"}, {"other": 1}]}`)
	got := Collect(doc, "running[].url")
	want := []any{"http://a", "http://b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if Collect(doc, "nothing[].url") != nil {
		t.Fatal("a missing list must give nil")
	}
}
