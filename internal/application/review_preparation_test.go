package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/abietic/argus/internal/reviewcore"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestReviewPreparationSchemaExampleAndDecoder(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	schemaData, err := os.ReadFile("../../api/schema/v1alpha1/review-preparation.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	resource, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaData))
	if err != nil {
		t.Fatal(err)
	}
	uri := "https://argus.local/schema/v1alpha1/review-preparation.schema.json"
	if err := compiler.AddResource(uri, resource); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile(uri)
	if err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile("../../examples/review-preparation.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeReviewInputPreparation(example); err != nil {
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
		"unknown nested field": func(value map[string]any) { value["input_ref"].(map[string]any)["unknown"] = true },
		"wrong contract":       func(value map[string]any) { value["input_ref"].(map[string]any)["contract"] = "wrong" },
		"invalid digest":       func(value map[string]any) { value["request_sha256"] = "invalid" },
		"relative repository":  func(value map[string]any) { value["repository_root"] = "relative" },
		"null receipts":        func(value map[string]any) { value["context_provider_receipt_refs"] = nil },
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
			if _, err := decodeReviewInputPreparation(data); err == nil {
				t.Fatal("decoder accepted invalid preparation")
			}
			if err := schema.Validate(value); err == nil {
				t.Fatal("schema accepted invalid preparation")
			}
		})
	}
	if _, err := decodeReviewInputPreparation(append(example, []byte(" {}")...)); err == nil {
		t.Fatal("decoder accepted trailing JSON")
	}
}

func TestPreparedDiffAndSelectionRestartBeforeCreatedWithoutReadingSource(t *testing.T) {
	for _, mode := range []reviewcore.TargetMode{reviewcore.TargetModeDiff, reviewcore.TargetModeSelection} {
		t.Run(string(mode), func(t *testing.T) {
			repositoryPath, base, head := reviewFixture(t)
			runID := "run-prepared-source"
			request := ReviewRequest{RepositoryPath: repositoryPath, Mode: mode}
			if mode == reviewcore.TargetModeDiff {
				request.BaseRevision, request.HeadRevision = base, head
			} else {
				request.Revision, request.SelectionPath = head, "review.go"
				request.StartLine, request.EndLine = 3, 4
			}
			dispatch := recoveryTestDispatch(runID, "artifact://local/sha256/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
			ids := &preparedReviewTestIDs{runID: runID, prefix: "first"}
			injected := errors.New("injected lifecycle boundary")
			first, runs := newTestService(t, ServiceOptions{
				IDs: ids, ExecutionDispatch: &dispatch,
				ExecutionAuthority: func(context.Context, scheduling.Dispatch) error {
					if ids.snapshot {
						return injected
					}
					return nil
				},
			})
			if _, err := first.Review(t.Context(), request); !errors.Is(err, injected) {
				t.Fatalf("first review error=%v", err)
			}
			frozenRef, err := runs.ReviewPreparation(runID)
			if err != nil {
				t.Fatal(err)
			}
			before, err := runs.Events(runID)
			if err != nil || len(before) != 0 {
				t.Fatalf("precreated events=%v error=%v", before, err)
			}
			second, err := NewService(panicTargetSource{}, runs, ServiceOptions{
				IDs: &preparedReviewTestIDs{runID: runID, prefix: "second"}, BuildIdentity: first.buildIdentity,
				DisableScheduling: true, ExecutionDispatch: &dispatch,
			})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := second.Review(t.Context(), request)
			if err != nil || outcome.Run.Status != runmodel.RunStatusSucceeded {
				t.Fatalf("restart outcome=%+v error=%v", outcome, err)
			}
			afterRef, err := runs.ReviewPreparation(runID)
			if err != nil || afterRef != frozenRef {
				t.Fatalf("restart replaced prepared input: ref=%+v error=%v", afterRef, err)
			}
		})
	}
}

type preparedReviewTestIDs struct {
	runID, prefix string
	sequence      int
	snapshot      bool
}

func (ids *preparedReviewTestIDs) New(prefix string) (string, error) {
	if prefix == "run" {
		return ids.runID, nil
	}
	ids.sequence++
	if prefix == "snapshot" {
		ids.snapshot = true
	}
	return fmt.Sprintf("%s-%s-%d", prefix, ids.prefix, ids.sequence), nil
}
