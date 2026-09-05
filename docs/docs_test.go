package docs_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/arasHi87/gogolook-task-api/docs"
)

// The four required operations, on both the canonical and the unversioned
// surface. If someone renames an RPC and forgets its annotation, the spec stops
// describing the contract the exercise asked for — and this fails.
func TestSpecDescribesTheRequiredContract(t *testing.T) {
	t.Parallel()

	raw, err := docs.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}

	var doc struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"info"`
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			SecuritySchemes map[string]any `json:"securitySchemes"`
		} `json:"components"`
		Security []map[string]any `json:"security"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("embedded spec is not valid JSON: %v", err)
	}

	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Errorf("openapi = %q, want 3.1.x", doc.OpenAPI)
	}
	if doc.Info.Title == "" || doc.Info.Version == "" {
		t.Errorf("info = %+v, want a title and a version", doc.Info)
	}

	want := map[string][]string{
		"/api/v1/tasks":      {"get", "post"},
		"/api/v1/tasks/{id}": {"put", "delete"},
		"/tasks":             {"get", "post"},
		"/tasks/{id}":        {"put", "delete"},
	}
	for path, methods := range want {
		ops, ok := doc.Paths[path]
		if !ok {
			t.Errorf("spec is missing path %s", path)
			continue
		}
		for _, m := range methods {
			if _, ok := ops[m]; !ok {
				t.Errorf("spec is missing %s %s", strings.ToUpper(m), path)
			}
		}
	}

	if _, ok := doc.Components.SecuritySchemes["bearerAuth"]; !ok {
		t.Error("spec declares no bearerAuth scheme, so the docs console has no auth box")
	}

	// The empty security requirement is what says "anonymous is allowed", and
	// it has to match auth.mode: optional.
	anonymous := false
	for _, req := range doc.Security {
		if len(req) == 0 {
			anonymous = true
		}
	}
	if !anonymous {
		t.Error("spec has no empty security requirement; it must say anonymous callers are allowed")
	}
}

// The status enum and the name length are declared once in the proto. If they
// stop reaching the document, clients lose the only machine-readable statement
// of the contract.
func TestSpecCarriesValidationConstraints(t *testing.T) {
	t.Parallel()

	raw, err := docs.JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	// Decoded loosely: protojson renders an int64 as `type: [integer, string]`,
	// so a schema's type is not always a plain string.
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]map[string]any `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}

	task, ok := doc.Components.Schemas["task.v1.Task"]
	if !ok {
		t.Fatalf("spec has no task.v1.Task schema; schemas: %v", keys(doc.Components.Schemas))
	}

	status := task.Properties["status"]
	if status["type"] != "integer" {
		t.Errorf("status type = %v, want integer: the contract specifies an integer, not an enum name", status["type"])
	}
	if got := fmt.Sprint(status["enum"]); got != "[0 1]" {
		t.Errorf("status enum = %v, want [0 1]", status["enum"])
	}

	name := task.Properties["name"]
	if name["maxLength"] != float64(255) {
		t.Errorf("name maxLength = %v, want 255", name["maxLength"])
	}
	if name["minLength"] != float64(1) {
		t.Errorf("name minLength = %v, want 1", name["minLength"])
	}
	if name["type"] != "string" {
		t.Errorf("name type = %v, want string", name["type"])
	}

	if task.Properties["id"]["format"] != "uuid" {
		t.Errorf("id format = %v, want uuid", task.Properties["id"]["format"])
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
