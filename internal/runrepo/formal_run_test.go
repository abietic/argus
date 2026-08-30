package runrepo

import (
	"testing"

	"argus.local/argus/internal/workflow"
)

func TestFormalWorkflowClassifierTracksCanonicalDefinition(t *testing.T) {
	definition := workflow.FormalAgentReviewDefinition()
	if !isFormalAgentWorkflow(definition) {
		t.Fatalf("canonical formal workflow %q was not classified as formal", definition.ID)
	}
	definition.ID = "deterministic-review"
	if isFormalAgentWorkflow(definition) {
		t.Fatal("non-formal workflow was classified as formal")
	}
}
