package runrepo

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/abietic/argus/internal/store/local"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestReviewPreparationBindingSurvivesRepositoryRestartAndRejectsChangedInput(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run-preparation-binding"
	ref, err := first.PutJSONArtifact(ReviewPreparationContract, map[string]string{"input": "frozen"})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if err := first.BindReviewPreparation(runID, ref, at); err != nil {
		t.Fatal(err)
	}
	second, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := second.ReviewPreparation(runID)
	if err != nil || loaded != ref {
		t.Fatalf("reopened preparation=%+v error=%v", loaded, err)
	}
	if err := second.BindReviewPreparation(runID, ref, at.Add(time.Second)); err != nil {
		t.Fatalf("exact input retry changed authority: %v", err)
	}
	changed, err := second.PutJSONArtifact(ReviewPreparationContract, map[string]string{"input": "different"})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.BindReviewPreparation(runID, changed, at.Add(2*time.Second)); !errors.Is(err, local.ErrEventConflict) {
		t.Fatalf("changed preparation was admitted: %v", err)
	}
	stream, err := reviewPreparationStream(runID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadJSONL(stream)
	if err != nil || len(events) != 1 || !events[0].Time.Equal(at) {
		t.Fatalf("preparation binding changed after retries: events=%+v error=%v", events, err)
	}
	history, err := second.History(0)
	if err != nil || len(history) != 0 {
		t.Fatalf("preparation claimed run lifecycle authority: history=%+v error=%v", history, err)
	}
}

func TestReviewPreparationBindingSchemaExampleAndDecoder(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	for _, name := range []string{"review-preparation.schema.json", "review-preparation-binding.schema.json"} {
		data, err := os.ReadFile("../../api/schema/v1alpha1/" + name)
		if err != nil {
			t.Fatal(err)
		}
		resource, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if err := compiler.AddResource("https://argus.local/schema/v1alpha1/"+name, resource); err != nil {
			t.Fatal(err)
		}
	}
	schema, err := compiler.Compile("https://argus.local/schema/v1alpha1/review-preparation-binding.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile("../../examples/review-preparation-binding.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeReviewPreparationBinding(example); err != nil {
		t.Fatal(err)
	}
	value, err := jsonschema.UnmarshalJSON(bytes.NewReader(example))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(value); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"unknown field":        func(value map[string]any) { value["unknown"] = true },
		"unknown nested field": func(value map[string]any) { value["preparation_ref"].(map[string]any)["unknown"] = true },
		"wrong contract":       func(value map[string]any) { value["preparation_ref"].(map[string]any)["contract"] = "wrong" },
		"missing identity":     func(value map[string]any) { delete(value, "run_id") },
		"null artifact":        func(value map[string]any) { value["preparation_ref"] = nil },
	} {
		t.Run(name, func(t *testing.T) {
			var value map[string]any
			if err := json.Unmarshal(example, &value); err != nil {
				t.Fatal(err)
			}
			mutate(value)
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeReviewPreparationBinding(data); err == nil {
				t.Fatal("decoder accepted invalid binding")
			}
			if err := schema.Validate(value); err == nil {
				t.Fatal("schema accepted invalid binding")
			}
		})
	}
	if _, err := decodeReviewPreparationBinding(append(example, []byte(" {}")...)); err == nil {
		t.Fatal("decoder accepted trailing JSON")
	}
}
