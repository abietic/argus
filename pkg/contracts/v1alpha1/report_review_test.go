package v1alpha1

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestReportReviewContractsAndExamples(t *testing.T) {
	for _, test := range []struct {
		name   string
		decode func([]byte) error
		bad    map[string]any
	}{
		{
			name:   "report-review-request",
			decode: func(b []byte) error { _, err := DecodeReportReviewRequest(b); return err },
			bad: map[string]any{
				"profile": "unknown.v1", "task_revision": 0, "artifact_sha256": "wrong",
				"evidence_sha256": map[string]string{"../facts.json": "anything"},
			},
		},
		{
			name:   "report-review-result",
			decode: func(b []byte) error { _, err := DecodeReportReviewResult(b); return err },
			bad: map[string]any{
				"verdict": "passed", "findings": nil, "request_sha256": "wrong",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join("..", "..", "..")
			data, err := os.ReadFile(filepath.Join(root, "api", "schema", "v1alpha1", test.name+".schema.json"))
			if err != nil {
				t.Fatal(err)
			}
			resource, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			compiler := jsonschema.NewCompiler()
			uri := "https://argus.local/schema/v1alpha1/" + test.name + ".schema.json"
			if err := compiler.AddResource(uri, resource); err != nil {
				t.Fatal(err)
			}
			schema, err := compiler.Compile(uri)
			if err != nil {
				t.Fatal(err)
			}
			example, err := os.ReadFile(filepath.Join(root, "examples", test.name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			value, err := jsonschema.UnmarshalJSON(bytes.NewReader(example))
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(value); err != nil {
				t.Fatal(err)
			}
			if err := test.decode(example); err != nil {
				t.Fatal(err)
			}
			marketName := "report-review-market-" + strings.TrimPrefix(test.name, "report-review-") + ".json"
			market, err := os.ReadFile(filepath.Join(root, "examples", marketName))
			if err != nil {
				t.Fatal(err)
			}
			marketValue, err := jsonschema.UnmarshalJSON(bytes.NewReader(market))
			if err != nil {
				t.Fatal(err)
			}
			if schema.Validate(marketValue) != nil || test.decode(market) != nil {
				t.Fatal("market profile example rejected")
			}
			for field, replacement := range test.bad {
				object := value.(map[string]any)
				original := object[field]
				object[field] = replacement
				invalid, err := json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
				if schema.Validate(object) == nil || test.decode(invalid) == nil {
					t.Errorf("accepted invalid field %s", field)
				}
				object[field] = original
			}
			for _, invalid := range [][]byte{
				append([]byte(`{"task_revision":4,`), example[1:]...),
				bytes.Replace(example, []byte(`"task_id"`), []byte(`"TASK_ID"`), 1),
				append(bytes.Clone(example), []byte(`{}`)...),
			} {
				if test.decode(invalid) == nil {
					t.Errorf("strict decoder accepted %s", invalid)
				}
			}
		})
	}
}
