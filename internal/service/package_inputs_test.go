package service

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// mustInputs are the inputs of schema, read by the compiler.
func mustInputs(t *testing.T, schema map[string]any) []models.PackageInput {
	t.Helper()
	inputs, err := inputsFromMarkers(schema)
	if err != nil {
		t.Fatal(err)
	}
	return inputs
}

// A reference the compile does not reach, under a combinator, is refused
// as the compile refuses it.
func TestInputsFromMarkersRefusesAReferenceUnderACombinator(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{"db": map[string]any{
		"anyOf": []any{map[string]any{"type": "string", "x-okdp-connection-ref": map[string]any{"contract": "database-server"}}},
	}}}
	if _, err := inputsFromMarkers(schema); err == nil {
		t.Error("a reference under anyOf is out of the compile's reach")
	}
}

func TestInputsFromMarkersReadsAConnectionRef(t *testing.T) {
	var schema map[string]any
	raw := `{
  "required": ["metadataDb"],
  "properties": {
    "workers": {"type": "integer"},
    "metadataDb": {"type": "string", "description": "Metadata database", "x-okdp-connection-ref": {"contract": "database-server"}},
    "pgConnection": {"type": "string", "default": "shared-db", "x-okdp-connection-ref": {"contract": "database-server"}},
    "legacy": {"type": "string", "x-kubocd-connection-ref": {"contract": "database-server"}}
  }
}`
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatal(err)
	}

	inputs := mustInputs(t, schema)

	if len(inputs) != 2 {
		t.Fatalf("got %d inputs, want 2 (the KuboCD marker is gone): %+v", len(inputs), inputs)
	}
	// Sorted by parameter name.
	if inputs[0].Parameter != "metadataDb" || inputs[0].Path != "metadataDb" || inputs[0].Optional {
		t.Errorf("metadataDb = %+v, want required (listed in the parent's required array)", inputs[0])
	}
	if inputs[0].Contract != "database-server" || inputs[0].Description != "Metadata database" {
		t.Errorf("metadataDb detail = %+v", inputs[0])
	}
	if inputs[1].Parameter != "pgConnection" || !inputs[1].Optional || inputs[1].Default != "shared-db" {
		t.Errorf("pgConnection = %+v, want optional with its default", inputs[1])
	}
}

func TestInputsFromMarkersToleratesAChartWithout(t *testing.T) {
	schema := map[string]any{"properties": map[string]any{"workers": map[string]any{"type": "integer"}}}
	if got := mustInputs(t, schema); got != nil {
		t.Errorf("got %v, want nil", got)
	}
}

func TestParameterSchemaDropsThePlatformKeys(t *testing.T) {
	chart := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"global", "workers"},
		"properties": map[string]any{
			"global":      map[string]any{"type": "object"},
			"connections": map[string]any{"type": "object"},
			"workers":     map[string]any{"type": "integer", "title": "Workers", "x-ui-group": "Sizing"},
		},
	}
	got := parameterSchema(chart)
	props := got["properties"].(map[string]any)
	if _, ok := props["global"]; ok {
		t.Errorf("global is filled by the platform, not the form")
	}
	if _, ok := props["connections"]; ok {
		t.Errorf("connections are layered from files, not the form")
	}
	workers := props["workers"].(map[string]any)
	if workers["x-ui-group"] != "Sizing" || workers["title"] != "Workers" {
		t.Errorf("the UI keywords must pass through untouched: %v", workers)
	}
	if req := got["required"].([]any); len(req) != 1 || req[0] != "workers" {
		t.Errorf("required = %v", req)
	}
	// The chart's own document is not modified.
	if _, ok := chart["properties"].(map[string]any)["global"]; !ok {
		t.Errorf("parameterSchema mutated its input")
	}
}

// The trino chart of the platform-packages spike (a copy of its
// values.schema.json): refs live in the items of catalog lists.
func TestInputsFromMarkersWalksNestedRefs(t *testing.T) {
	raw, err := os.ReadFile("testdata/trino.values.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var chart map[string]any
	if err := json.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	schema := parameterSchema(chart, "okdp-lib")
	for _, reserved := range []string{"global", "connections", "okdp-lib"} {
		if _, ok := schema["properties"].(map[string]any)[reserved]; ok {
			t.Errorf("%s is offered as a form field", reserved)
		}
	}

	inputs := mustInputs(t, schema)
	got := map[string]string{}
	for _, in := range inputs {
		got[in.Path] = in.Contract
		if in.Parameter != "" {
			t.Errorf("%s is nested, it names no root parameter: %+v", in.Path, in)
		}
	}
	want := map[string]string{
		"hiveCatalogs[].metastore":  "hive",
		"hiveCatalogs[].storage":    "s3",
		"icebergCatalogs[].catalog": "iceberg-catalog",
		"icebergCatalogs[].storage": "s3",
	}
	if len(got) != len(want) {
		t.Fatalf("inputs = %v, want %v", got, want)
	}
	for p, c := range want {
		if got[p] != c {
			t.Errorf("%s: contract %q, want %q", p, got[p], c)
		}
	}
}

func TestInputsFromMarkersReadsAClaimTemplate(t *testing.T) {
	var schema map[string]any
	raw := `{
  "properties": {
    "db": {"type": ["string", "object"], "default": {},
           "x-okdp-connection-ref": {"contract": "database-server", "provision": true,
             "request": {"extensions": ["vector"]}, "require": {"required": ["secretRef"]}}},
    "store": {"type": ["string", "object"], "default": {"class": "fast"},
              "x-okdp-connection-ref": {"contract": "s3", "provision": true}}
  }
}`
	if err := json.Unmarshal([]byte(raw), &schema); err != nil {
		t.Fatal(err)
	}
	inputs := mustInputs(t, schema)
	if len(inputs) != 2 {
		t.Fatalf("got %d inputs, want 2: %+v", len(inputs), inputs)
	}
	db := inputs[0]
	if !db.Provision || db.DefaultClaim == nil || db.Default != "" {
		t.Errorf("db = %+v, want a claim template by default", db)
	}
	if db.Request["extensions"] == nil || db.Require["required"] == nil {
		t.Errorf("db = %+v, want the package's request and requirement", db)
	}
	if inputs[1].DefaultClaim == nil || inputs[1].DefaultClaim.Class != "fast" {
		t.Errorf("store = %+v, want the class of its claim template", inputs[1])
	}
}
