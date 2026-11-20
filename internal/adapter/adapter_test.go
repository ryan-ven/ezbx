package adapter

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ryan-ven/ezbx/providers"
)

// The adapters that ship inside ezbx must all load and pass validation.
func TestEmbeddedAdaptersLoad(t *testing.T) {
	adapters, err := LoadFS(providers.FS)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range adapters {
		ids = append(ids, a.ID)
	}
	if got := strings.Join(ids, ","); got != "llamacpp,lmstudio,ollama,openai,strata" {
		t.Fatalf("unexpected adapters: %s", got)
	}
}

const good = `{
  "schema": 1, "id": "demo", "name": "Demo",
  "detect": {"hintPorts": [1], "cli": null, "probes": [{"path": "/v1/models", "expect": {"hasFields": ["data[].id"]}}]},
  "openai": {"basePath": "/v1", "modelsPath": "/v1/models", "chatPath": "/v1/chat/completions"},
  "models": {"id": "data[].id", "contextWindow": null, "input": null, "loaded": null},
  "auth": {"mode": "none", "keyEnv": null, "note": ""},
  "capabilities": [], "control": {}, "verified": {"status": "docs-only", "on": null, "notes": ""}
}`

func load(t *testing.T, folder, body string) error {
	t.Helper()
	_, err := LoadFS(fstest.MapFS{folder + "/adapter.json": {Data: []byte(body)}})
	return err
}

func TestValidation(t *testing.T) {
	if err := load(t, "demo", good); err != nil {
		t.Fatalf("a good adapter must load: %v", err)
	}

	bad := map[string]string{
		"id does not match folder": strings.Replace(good, `"id": "demo"`, `"id": "other"`, 1),
		"unknown field (typo)":     strings.Replace(good, `"schema": 1,`, `"schema": 1, "hintports": [1],`, 1),
		"wrong schema":             strings.Replace(good, `"schema": 1`, `"schema": 2`, 1),
		"probe path without slash": strings.Replace(good, `"path": "/v1/models"`, `"path": "v1/models"`, 1),
		"no required probe":        strings.Replace(good, `"expect"`, `"optional": true, "expect"`, 1),
		"missing models.id":        strings.Replace(good, `"id": "data[].id"`, `"id": ""`, 1),
		"models.id not in a list":  strings.Replace(good, `"id": "data[].id"`, `"id": "id"`, 1),
		"loaded form unreadable":   strings.Replace(good, `"loaded": null`, `"loaded": "/api/ps models[].name extra"`, 1),
		"loaded field elsewhere":   strings.Replace(good, `"loaded": null`, `"loaded": "other[].state == loaded"`, 1),
		"loaded without value":     strings.Replace(good, `"loaded": null`, `"loaded": "data[].state == "`, 1),
		"loaded plain word":        strings.Replace(good, `"loaded": null`, `"loaded": "yes"`, 1),
		"context outside the list": strings.Replace(good, `"contextWindow": null`, `"contextWindow": "meta.n_ctx"`, 1),
		"probe expects nothing":    strings.Replace(good, `"expect": {"hasFields": ["data[].id"]}`, `"expect": {}`, 1),
		"hasLists with brackets":   strings.Replace(good, `"expect": {"hasFields": ["data[].id"]}`, `"expect": {"hasLists": ["data[]"]}`, 1),
	}
	for name, body := range bad {
		if err := load(t, "demo", body); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}

func TestLoadedForms(t *testing.T) {
	for _, form := range []string{`"data[].status.value == loaded"`, `"/api/ps models[].name"`} {
		if err := load(t, "demo", strings.Replace(good, `"loaded": null`, `"loaded": `+form, 1)); err != nil {
			t.Errorf("loaded %s should be accepted: %v", form, err)
		}
	}
	if err := load(t, "demo", strings.Replace(good, `"expect": {"hasFields": ["data[].id"]}`, `"expect": {"hasLists": ["data"]}`, 1)); err != nil {
		t.Errorf("hasLists should be accepted: %v", err)
	}
}
