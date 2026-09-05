package reviewjob

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestReviewJobRequestSchemaAndStrictDecoderAgreeOnFrozenInputs(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	for _, filename := range []string{"review-spec.schema.json", "review-input.schema.json", "review-job-request.schema.json"} {
		data, err := os.ReadFile("../../api/schema/v1alpha1/" + filename)
		if err != nil {
			t.Fatal(err)
		}
		resource, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.AddResource("https://argus.local/schema/v1alpha1/"+filename, resource); err != nil {
			t.Fatal(err)
		}
	}
	schema, err := compiler.Compile("https://argus.local/schema/v1alpha1/review-job-request.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("../../examples/local-api-review-job-submit-command.json")
	if err != nil {
		t.Fatal(err)
	}
	var example struct {
		Request json.RawMessage `json:"request"`
	}
	if err := json.Unmarshal(data, &example); err != nil {
		t.Fatal(err)
	}
	var request Request
	if err := decodeStrict(example.Request, &request); err != nil {
		t.Fatal(err)
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(example.Request))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(value); err != nil {
		t.Fatalf("example schema validation: %v", err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"unknown context field": func(value map[string]any) {
			value["contexts"].([]any)[0].(map[string]any)["unknown"] = true
		},
		"invalid context union": func(value map[string]any) {
			value["contexts"] = []any{map[string]any{}}
		},
		"CRLF overlay":  func(value map[string]any) { value["overlay_content"] = "line\r\n" },
		"NUL overlay":   func(value map[string]any) { value["overlay_content"] = "line\x00" },
		"null overlay":  func(value map[string]any) { value["overlay_content"] = nil },
		"null contexts": func(value map[string]any) { value["contexts"] = nil },
		"formal secondary input": func(value map[string]any) {
			for key := range value {
				delete(value, key)
			}
			value["schema_version"] = RequestSchemaVersion
			value["execution_profile"] = FormalPiExecutionProfile
			value["source_run_id"] = "run-source"
			value["execution_timeout_seconds"] = 1800
			value["contexts"] = []any{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var invalid map[string]any
			if err := json.Unmarshal(example.Request, &invalid); err != nil {
				t.Fatal(err)
			}
			mutate(invalid)
			data, err := json.Marshal(invalid)
			if err != nil {
				t.Fatal(err)
			}
			value, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(value); err == nil {
				t.Fatal("schema accepted invalid request")
			}
			var decoded Request
			if err := decodeStrict(data, &decoded); err == nil && decoded.Validate() == nil {
				t.Fatal("strict decoder and validator accepted invalid request")
			}
		})
	}
}
